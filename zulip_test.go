package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Placeholder fixtures only — this is a public repo, so no real site, address,
// key or stream name appears in a test.
const (
	testBotEmail = "notetaker-bot@zulip.example.com"
	testAPIKey   = "test-api-key"
	testStream   = "test-stream"
	testTopic    = "test-topic"
	testRoomURL  = "https://meet.jit.si/fakeroom"
	testContent  = "[Join video call.](" + testRoomURL + ")"
)

type capturedReq struct {
	method, path string
	query        url.Values
	form         url.Values
	user, pass   string
}

// zulipServer is an httptest.Server that records every request before handing
// it to the test's handler.
type zulipServer struct {
	*httptest.Server
	handler func(w http.ResponseWriter, r *http.Request, form url.Values)

	mu   sync.Mutex
	reqs []capturedReq
}

func newZulipServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, form url.Values)) (*Zulip, *zulipServer) {
	t.Helper()
	s := &zulipServer{handler: h}
	s.Server = httptest.NewServer(s)
	t.Cleanup(s.Close)
	return newZulip(Config{
		ZulipSite:      s.URL,
		ZulipBotEmail:  testBotEmail,
		ZulipBotAPIKey: testAPIKey,
	}), s
}

func (s *zulipServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	form := requestForm(r)
	user, pass, _ := r.BasicAuth()
	s.mu.Lock()
	s.reqs = append(s.reqs, capturedReq{
		method: r.Method, path: r.URL.Path, query: r.URL.Query(), form: form, user: user, pass: pass,
	})
	s.mu.Unlock()
	s.handler(w, r, form)
}

func (s *zulipServer) requests() []capturedReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedReq(nil), s.reqs...)
}

// requestForm reads the url-encoded body. net/http's ParseForm only does this
// for POST/PUT/PATCH, and the remove-reaction call is a DELETE with a body.
func requestForm(r *http.Request) url.Values {
	b, _ := io.ReadAll(r.Body)
	v, err := url.ParseQuery(string(b))
	if err != nil {
		return url.Values{}
	}
	return v
}

// ok answers with a Zulip success body.
func ok(w http.ResponseWriter, body string) {
	if body == "" {
		body = `{"result":"ok"}`
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}

func TestZulipMe(t *testing.T) {
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		ok(w, `{"result":"ok","user_id":7}`)
	})
	id, err := z.Me(context.Background())
	if err != nil || id != 7 {
		t.Fatalf("Me() = %d, %v; want 7, nil", id, err)
	}
	req := s.requests()[0]
	if req.method != http.MethodGet || req.path != "/api/v1/users/me" {
		t.Errorf("got %s %s; want GET /api/v1/users/me", req.method, req.path)
	}
	if req.user != testBotEmail || req.pass != testAPIKey {
		t.Errorf("basic auth = %q/%q; want the bot email and key", req.user, req.pass)
	}
}

func TestZulipRegisterSendsTheRightForm(t *testing.T) {
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		ok(w, `{"result":"ok","queue_id":"q1","last_event_id":42}`)
	})
	queue, last, err := z.Register(context.Background())
	if err != nil || queue != "q1" || last != 42 {
		t.Fatalf(`Register() = %q, %d, %v; want "q1", 42, nil`, queue, last, err)
	}
	req := s.requests()[0]
	if req.method != http.MethodPost || req.path != "/api/v1/register" {
		t.Errorf("got %s %s; want POST /api/v1/register", req.method, req.path)
	}
	if got := req.form.Get("event_types"); got != `["message","reaction"]` {
		t.Errorf("event_types = %q", got)
	}
	// Raw content in message events means no extra fetch per message.
	if got := req.form.Get("apply_markdown"); got != "false" {
		t.Errorf("apply_markdown = %q; want false", got)
	}
	if req.user != testBotEmail || req.pass != testAPIKey {
		t.Errorf("basic auth = %q/%q; want the bot email and key", req.user, req.pass)
	}
}

func TestZulipEventsDecodesBothEventKinds(t *testing.T) {
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		ok(w, `{"result":"ok","events":[
			{"id":1,"type":"message","message":{"id":100,"type":"stream","content":"hi","display_recipient":"`+testStream+`","subject":"`+testTopic+`","sender_id":42}},
			{"id":2,"type":"reaction","op":"add","user_id":42,"message_id":100,"emoji_name":"`+micEmoji+`"}
		]}`)
	})
	events, err := z.Events(context.Background(), "q1", 42)
	if err != nil {
		t.Fatalf("Events() error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events; want 2", len(events))
	}
	if m := events[0].Message; m == nil || m.ID != 100 || m.Type != "stream" ||
		m.DisplayRecipient != testStream || m.Subject != testTopic || m.SenderID != 42 {
		t.Errorf("message event decoded as %+v", m)
	}
	if e := events[1]; e.Op != "add" || e.UserID != 42 || e.MessageID != 100 || e.EmojiName != micEmoji {
		t.Errorf("reaction event decoded as %+v", e)
	}
	req := s.requests()[0]
	if req.method != http.MethodGet || req.path != "/api/v1/events" {
		t.Errorf("got %s %s; want GET /api/v1/events", req.method, req.path)
	}
	if req.query.Get("queue_id") != "q1" || req.query.Get("last_event_id") != "42" {
		t.Errorf("query = %v; want queue_id=q1 last_event_id=42", req.query)
	}
}

func TestZulipEventsExpiredQueue(t *testing.T) {
	z, _ := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"result":"error","code":"BAD_EVENT_QUEUE_ID","msg":"Bad event queue id: q1","queue_id":"q1"}`)
	})
	_, err := z.Events(context.Background(), "q1", 0)
	if !errors.Is(err, errBadQueue) {
		t.Fatalf("Events() error = %v; want errBadQueue", err)
	}
}

func TestZulipGetMessage(t *testing.T) {
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		ok(w, `{"result":"ok","message":{"id":100,"type":"stream","content":"`+testContent+`","display_recipient":"`+testStream+`","subject":"`+testTopic+`","sender_id":42}}`)
	})
	m, err := z.GetMessage(context.Background(), 100)
	if err != nil {
		t.Fatalf("GetMessage() error: %v", err)
	}
	if m.ID != 100 || m.Type != "stream" || m.DisplayRecipient != testStream || m.Subject != testTopic {
		t.Errorf("got %+v", m)
	}
	req := s.requests()[0]
	if req.method != http.MethodGet || req.path != "/api/v1/messages/100" {
		t.Errorf("got %s %s; want GET /api/v1/messages/100", req.method, req.path)
	}
	// On this endpoint apply_markdown is a query parameter, not a form field.
	if got := req.query.Get("apply_markdown"); got != "false" {
		t.Errorf("apply_markdown = %q; want false", got)
	}
}

// A DM's display_recipient is an array of users, not a stream name. Decoding
// must not fail: one DM would otherwise poison a whole event batch.
func TestZulipGetMessagePrivateRecipient(t *testing.T) {
	z, _ := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		ok(w, `{"result":"ok","message":{"id":101,"type":"private","content":"hi","display_recipient":[{"id":7},{"id":42}],"subject":"","sender_id":42}}`)
	})
	m, err := z.GetMessage(context.Background(), 101)
	if err != nil {
		t.Fatalf("GetMessage() error: %v", err)
	}
	if m.Type != "private" || m.DisplayRecipient != "" {
		t.Errorf("got type %q, recipient %q; want private and an empty stream", m.Type, m.DisplayRecipient)
	}
}

func TestZulipReactions(t *testing.T) {
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) { ok(w, "") })
	ctx := context.Background()
	if err := z.AddReaction(ctx, 100, micEmoji); err != nil {
		t.Fatalf("AddReaction() error: %v", err)
	}
	if err := z.RemoveReaction(ctx, 100, recordingEmoji); err != nil {
		t.Fatalf("RemoveReaction() error: %v", err)
	}
	reqs := s.requests()
	if reqs[0].method != http.MethodPost || reqs[0].path != "/api/v1/messages/100/reactions" ||
		reqs[0].form.Get("emoji_name") != micEmoji {
		t.Errorf("add: %s %s %v", reqs[0].method, reqs[0].path, reqs[0].form)
	}
	if reqs[1].method != http.MethodDelete || reqs[1].path != "/api/v1/messages/100/reactions" ||
		reqs[1].form.Get("emoji_name") != recordingEmoji {
		t.Errorf("remove: %s %s %v", reqs[1].method, reqs[1].path, reqs[1].form)
	}
}

func TestZulipSendMessage(t *testing.T) {
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) { ok(w, "") })
	if err := z.SendMessage(context.Background(), testStream, testTopic, "hello"); err != nil {
		t.Fatalf("SendMessage() error: %v", err)
	}
	req := s.requests()[0]
	if req.method != http.MethodPost || req.path != "/api/v1/messages" {
		t.Errorf("got %s %s; want POST /api/v1/messages", req.method, req.path)
	}
	for k, want := range map[string]string{"type": "stream", "to": `"` + testStream + `"`, "topic": testTopic, "content": "hello"} {
		if got := req.form.Get(k); got != want {
			t.Errorf("form %s = %q; want %q", k, got, want)
		}
	}
}

// Zulip reads a bare integer "to" as a channel id, so a stream whose name looks
// like a number must go over as a JSON string or the message is misrouted.
func TestZulipSendMessageQuotesNumericStreamNames(t *testing.T) {
	z, s := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) { ok(w, "") })
	if err := z.SendMessage(context.Background(), "2026", testTopic, "hello"); err != nil {
		t.Fatalf("SendMessage() error: %v", err)
	}
	if got := s.requests()[0].form.Get("to"); got != `"2026"` {
		t.Errorf(`to = %s; want "2026" json-encoded`, got)
	}
}

func TestZulipErrorsCarryZulipsMessageNotTheKey(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		want       string
	}{
		{"result error", `{"result":"error","msg":"Invalid API key"}`, http.StatusForbidden, "Invalid API key"},
		{"non-2xx without a json body", "gateway down", http.StatusBadGateway, "http 502"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z, _ := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			err := z.AddReaction(context.Background(), 100, micEmoji)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("error leaks the api key: %v", err)
			}
		})
	}
}
