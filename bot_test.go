package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testBotID = 7

// botFixture is a Bot wired to an httptest Zulip that serves one message and
// accepts reactions, plus the jobs its start func received.
type botFixture struct {
	bot  *Bot
	srv  *zulipServer
	mu   sync.Mutex
	jobs []Job
}

// newBotFixture builds a bot whose GetMessage returns msg. startErr is what the
// runner seam reports back.
func newBotFixture(t *testing.T, jitsiBase string, msg Message, startErr error) *botFixture {
	t.Helper()
	f := &botFixture{}
	z, srv := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/messages/") && r.Method == http.MethodGet {
			ok(w, `{"result":"ok","message":`+mustJSON(t, msg)+`}`)
			return
		}
		ok(w, "")
	})
	f.srv = srv
	f.bot = newBot(Config{JitsiBaseURL: jitsiBase}, z, func(j Job) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.jobs = append(f.jobs, j)
		return startErr
	})
	f.bot.botID = testBotID
	return f
}

// reactions returns the emoji names added, in request order.
func (f *botFixture) reactions() []string {
	var out []string
	for _, r := range f.srv.requests() {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, "/reactions") {
			out = append(out, r.form.Get("emoji_name"))
		}
	}
	return out
}

// removed returns the emoji names deleted, in request order.
func (f *botFixture) removed() []string {
	var out []string
	for _, r := range f.srv.requests() {
		if r.method == http.MethodDelete && strings.HasSuffix(r.path, "/reactions") {
			out = append(out, r.form.Get("emoji_name"))
		}
	}
	return out
}

func (f *botFixture) startedJobs() []Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Job(nil), f.jobs...)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func streamMessage() Message {
	return Message{
		ID: 100, Type: "stream", Content: testContent,
		DisplayRecipient: testStream, Subject: testTopic, SenderID: 42,
	}
}

func TestBotHandle(t *testing.T) {
	msg := streamMessage()
	other := func(m Message, f func(*Message)) *Message { f(&m); return &m }

	tests := []struct {
		name      string
		event     Event
		wantEmoji []string
		wantJobs  int
	}{
		{
			name:      "a stream message with a call link gets the microphone reaction",
			event:     Event{Type: "message", Message: &msg},
			wantEmoji: []string{micEmoji},
		},
		{
			name:  "the bot's own message is ignored",
			event: Event{Type: "message", Message: other(msg, func(m *Message) { m.SenderID = testBotID })},
		},
		{
			name:  "a message without a call link is ignored",
			event: Event{Type: "message", Message: other(msg, func(m *Message) { m.Content = "lunch?" })},
		},
		{
			name:  "a private message is ignored",
			event: Event{Type: "message", Message: other(msg, func(m *Message) { m.Type = "private" })},
		},
		{
			name:      "a user's microphone reaction starts the job and marks it recording",
			event:     Event{Type: "reaction", Op: "add", EmojiName: micEmoji, UserID: 42, MessageID: 100},
			wantEmoji: []string{recordingEmoji},
			wantJobs:  1,
		},
		{
			name:  "the bot's own microphone reaction is ignored",
			event: Event{Type: "reaction", Op: "add", EmojiName: micEmoji, UserID: testBotID, MessageID: 100},
		},
		{
			name:  "removing the microphone reaction is ignored",
			event: Event{Type: "reaction", Op: "remove", EmojiName: micEmoji, UserID: 42, MessageID: 100},
		},
		{
			name:  "some other emoji is ignored",
			event: Event{Type: "reaction", Op: "add", EmojiName: "octopus", UserID: 42, MessageID: 100},
		},
		{
			name:  "an unrelated event type is ignored",
			event: Event{Type: "presence"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBotFixture(t, "https://meet.jit.si", msg, nil)
			f.bot.handle(context.Background(), tc.event)
			if got := f.reactions(); !reflect.DeepEqual(got, tc.wantEmoji) {
				t.Errorf("reactions = %v; want %v", got, tc.wantEmoji)
			}
			if got := len(f.startedJobs()); got != tc.wantJobs {
				t.Errorf("started %d jobs; want %d", got, tc.wantJobs)
			}
		})
	}
}

func TestBotStartsTheExpectedJob(t *testing.T) {
	f := newBotFixture(t, "https://meet.jit.si", streamMessage(), nil)
	f.bot.handle(context.Background(), Event{Type: "reaction", Op: "add", EmojiName: micEmoji, UserID: 42, MessageID: 100})

	jobs := f.startedJobs()
	if len(jobs) != 1 {
		t.Fatalf("started %d jobs; want 1", len(jobs))
	}
	want := Job{ID: "100", MessageID: 100, Stream: testStream, Topic: testTopic, JitsiURL: testRoomURL}
	if !reflect.DeepEqual(jobs[0], want) {
		t.Errorf("job = %+v; want %+v", jobs[0], want)
	}
}

// The indicator is added before the job starts, so a start that never took hold
// has to take it back off again. A duplicate click leaves it alone: the running
// job still owns it.
func TestBotStartFailures(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantRemoved []string
	}{
		{"a duplicate click leaves the running job's indicator alone", ErrDuplicateJob, nil},
		{"a failed start takes its own indicator back off", errors.New("disk full"), []string{recordingEmoji}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBotFixture(t, "https://meet.jit.si", streamMessage(), tc.err)
			f.bot.handle(context.Background(), Event{Type: "reaction", Op: "add", EmojiName: micEmoji, UserID: 42, MessageID: 100})
			if got := f.reactions(); !reflect.DeepEqual(got, []string{recordingEmoji}) {
				t.Errorf("reactions = %v; want [%s]", got, recordingEmoji)
			}
			if got := f.removed(); !reflect.DeepEqual(got, tc.wantRemoved) {
				t.Errorf("removed = %v; want %v", got, tc.wantRemoved)
			}
		})
	}
}

// A truncated room URL would silently record a different call, and a query or
// fragment can carry a JWT or room password that must never reach the job record.
func TestBotRoomURLExtraction(t *testing.T) {
	tests := []struct {
		name, content, want string
	}{
		{"the call button link", testContent, testRoomURL},
		{"a dot in the room name", "[Join video call.](https://meet.jit.si/team.sync)", "https://meet.jit.si/team.sync"},
		{"a percent-encoded room name", "https://meet.jit.si/team%20sync", "https://meet.jit.si/team%20sync"},
		{"a link at the end of a sentence", "we are in https://meet.jit.si/fakeroom.", testRoomURL},
		{"a jwt in the query is dropped", "https://meet.jit.si/fakeroom?jwt=secret-token", testRoomURL},
		{"a fragment is dropped", "https://meet.jit.si/fakeroom#config.startAudioOnly=true", testRoomURL},
		{"the bare base url is not a room", "https://meet.jit.si/", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := streamMessage()
			msg.Content = tc.content
			f := newBotFixture(t, "https://meet.jit.si", msg, nil)
			f.bot.handle(context.Background(), Event{Type: "reaction", Op: "add", EmojiName: micEmoji, UserID: 42, MessageID: 100})

			jobs := f.startedJobs()
			if tc.want == "" {
				if len(jobs) != 0 {
					t.Fatalf("started %+v; want no job", jobs)
				}
				return
			}
			if len(jobs) != 1 {
				t.Fatalf("started %d jobs; want 1", len(jobs))
			}
			if jobs[0].JitsiURL != tc.want {
				t.Errorf("JitsiURL = %q; want %q", jobs[0].JitsiURL, tc.want)
			}
			if strings.ContainsAny(jobs[0].JitsiURL, "?#") {
				t.Errorf("JitsiURL %q carries a query or fragment", jobs[0].JitsiURL)
			}
		})
	}
}

func TestBotIgnoresReactionsOnUnrecordableMessages(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
	}{
		{"not a stream message", Message{ID: 100, Type: "private", Content: testContent}},
		{"no call link", Message{ID: 100, Type: "stream", Content: "lunch?", DisplayRecipient: testStream}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newBotFixture(t, "https://meet.jit.si", tc.msg, nil)
			f.bot.handle(context.Background(), Event{Type: "reaction", Op: "add", EmojiName: micEmoji, UserID: 42, MessageID: 100})
			if got := f.startedJobs(); len(got) != 0 {
				t.Errorf("started %v; want no job", got)
			}
			if got := f.reactions(); len(got) != 0 {
				t.Errorf("reactions = %v; want none", got)
			}
		})
	}
}

// A self-hosted deployment sets JITSI_BASE_URL; links to any other host must not
// trigger a recording.
func TestBotHonoursCustomJitsiBaseURL(t *testing.T) {
	const base = "https://meet.example.com"
	msg := streamMessage()
	msg.Content = "[Join video call.](" + base + "/fakeroom)"

	f := newBotFixture(t, base, msg, nil)
	f.bot.handle(context.Background(), Event{Type: "reaction", Op: "add", EmojiName: micEmoji, UserID: 42, MessageID: 100})
	jobs := f.startedJobs()
	if len(jobs) != 1 || jobs[0].JitsiURL != base+"/fakeroom" {
		t.Fatalf("jobs = %+v; want one job on %s/fakeroom", jobs, base)
	}

	foreign := newBotFixture(t, base, msg, nil)
	foreign.bot.handle(context.Background(), Event{Type: "message", Message: &Message{
		ID: 101, Type: "stream", Content: testContent, DisplayRecipient: testStream, SenderID: 42,
	}})
	if got := foreign.reactions(); len(got) != 0 {
		t.Errorf("reactions = %v; want none for a link on another host", got)
	}
}

// Run must survive an expired event queue by registering a new one.
func TestBotRunReRegistersOnExpiredQueue(t *testing.T) {
	msg := streamMessage()
	reacted := make(chan struct{})
	var reactedOnce sync.Once
	var mu sync.Mutex
	registers, eventCalls := 0, 0

	z, srv := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		switch {
		case r.URL.Path == "/api/v1/users/me":
			ok(w, `{"result":"ok","user_id":7}`)
		case r.URL.Path == "/api/v1/register":
			mu.Lock()
			registers++
			n := registers
			mu.Unlock()
			ok(w, `{"result":"ok","queue_id":"q`+strconv.Itoa(n)+`","last_event_id":0}`)
		case r.URL.Path == "/api/v1/events":
			mu.Lock()
			eventCalls++
			n := eventCalls
			mu.Unlock()
			switch n {
			case 1: // the queue Zulip handed us has already expired
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"result":"error","code":"BAD_EVENT_QUEUE_ID","msg":"Bad event queue id"}`)
			case 2:
				ok(w, `{"result":"ok","events":[{"id":1,"type":"message","message":`+mustJSON(t, msg)+`}]}`)
			default: // long-poll: hold until the client goes away
				<-r.Context().Done()
			}
		case strings.HasSuffix(r.URL.Path, "/reactions"):
			ok(w, "")
			reactedOnce.Do(func() { close(reacted) })
		default:
			ok(w, "")
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bot := newBot(Config{JitsiBaseURL: "https://meet.jit.si"}, z, func(Job) error { return nil })
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()

	select {
	case <-reacted:
	case <-time.After(5 * time.Second):
		t.Fatal("no reaction was added within 5s")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v; want nil on a cancelled context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	mu.Lock()
	defer mu.Unlock()
	if registers != 2 {
		t.Errorf("registered %d times; want 2 (the initial one plus the retry)", registers)
	}
	if bot.botID != testBotID {
		t.Errorf("botID = %d; want %d", bot.botID, testBotID)
	}
	var added []string
	for _, r := range srv.requests() {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, "/reactions") {
			added = append(added, r.form.Get("emoji_name"))
		}
	}
	if len(added) != 1 || added[0] != micEmoji {
		t.Errorf("reactions = %v; want [%s]", added, micEmoji)
	}
}

func TestBotRunFailsOnBadCredentials(t *testing.T) {
	z, _ := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"result":"error","msg":"Invalid API key"}`)
	})
	err := newBot(Config{JitsiBaseURL: "https://meet.jit.si"}, z, func(Job) error { return nil }).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "zulip identity") {
		t.Fatalf("Run() = %v; want a zulip identity error", err)
	}
}
