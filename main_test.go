package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The wiring test stands in for the manual end-to-end run: a fake Zulip, the
// fake recorder from testdata/ and a local webhook receiver, driven through
// run() exactly as main does — 🎙️ offered, 🎙️ clicked, recording, 🔴 dropped,
// signed recording.finished delivered, then the signed /notify callback posting
// back into the same topic.

const (
	e2eMsgID  = int64(100)
	e2eSecret = "test-secret"
	e2eStream = "general"
	e2eTopic  = "standup"
	e2eRoom   = "https://example.invalid/RoomE2E"
)

// fakeZulipServer is the handful of REST endpoints the bot and the runner use.
type fakeZulipServer struct {
	mu        sync.Mutex
	reactions []string // "add:emoji" / "remove:emoji"
	posted    []zulipCall
	polls     int
	msgs      chan zulipCall
}

func newFakeZulipServer(t *testing.T) (*fakeZulipServer, string) {
	t.Helper()
	f := &fakeZulipServer{msgs: make(chan zulipCall, 4)}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	mux.HandleFunc("GET /api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"result":"success","user_id":7}`)
	})
	mux.HandleFunc("POST /api/v1/register", func(w http.ResponseWriter, _ *http.Request) {
		write(w, `{"result":"success","queue_id":"q1","last_event_id":0}`)
	})
	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.polls++
		n := f.polls
		f.mu.Unlock()
		switch n {
		case 1: // a call link posted by a human
			write(w, fmt.Sprintf(`{"result":"success","events":[{"id":1,"type":"message","message":`+
				`{"id":%d,"type":"stream","content":"Join video call: %s","display_recipient":%q,"subject":%q,"sender_id":9}}]}`,
				e2eMsgID, e2eRoom, e2eStream, e2eTopic))
		case 2: // a human clicks the microphone
			write(w, fmt.Sprintf(`{"result":"success","events":[{"id":2,"type":"reaction","op":"add",`+
				`"user_id":9,"message_id":%d,"emoji_name":%q}]}`, e2eMsgID, micEmoji))
		default: // idle long-poll
			time.Sleep(20 * time.Millisecond)
			write(w, `{"result":"success","events":[]}`)
		}
	})
	mux.HandleFunc("GET /api/v1/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		write(w, fmt.Sprintf(`{"result":"success","message":`+
			`{"id":%s,"type":"stream","content":"Join video call: %s","display_recipient":%q,"subject":%q,"sender_id":9}}`,
			r.PathValue("id"), e2eRoom, e2eStream, e2eTopic))
	})
	reaction := func(op string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// DELETE carries the form in the body, where r.FormValue never looks.
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			f.mu.Lock()
			f.reactions = append(f.reactions, op+":"+form.Get("emoji_name"))
			f.mu.Unlock()
			write(w, `{"result":"success"}`)
		}
	}
	mux.HandleFunc("POST /api/v1/messages/{id}/reactions", reaction("add"))
	mux.HandleFunc("DELETE /api/v1/messages/{id}/reactions", reaction("remove"))
	mux.HandleFunc("POST /api/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var to string
		_ = json.Unmarshal([]byte(r.FormValue("to")), &to)
		c := zulipCall{stream: to, topic: r.FormValue("topic"), content: r.FormValue("content")}
		f.mu.Lock()
		f.posted = append(f.posted, c)
		f.mu.Unlock()
		f.msgs <- c
		write(w, `{"result":"success","id":101}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeZulipServer) reactionLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reactions...)
}

// lockedBuf is a slog sink the wiring test can read while the service is still
// logging from its own goroutines.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunEndToEnd(t *testing.T) {
	// At INFO — what production runs at — the log has to tell an operator what
	// the bot did. A run that shows nothing between the queue registration and
	// the webhook is exactly the outage this asserts against.
	var logs lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	zulip, zulipURL := newFakeZulipServer(t)

	// The downstream receiver: it verifies the signature exactly as the real one
	// must, so a wrong header here fails the test rather than being ignored.
	delivered := make(chan []byte, 2)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get(headerEvent) != eventRecordingFinished {
			t.Errorf("event header = %q", r.Header.Get(headerEvent))
		}
		if !verifySignature(body, r.Header.Get(headerSignature), e2eSecret) {
			t.Error("webhook signature did not verify")
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		delivered <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	dataDir := t.TempDir()
	cfg := Config{
		ZulipSite:          zulipURL,
		ZulipBotEmail:      "bot@example.invalid",
		ZulipBotAPIKey:     "not-a-real-key",
		JitsiBaseURL:       "https://example.invalid",
		DataDir:            dataDir,
		HostDataDir:        dataDir,
		RecorderPath:       filepath.Join("testdata", "rec_ok.sh"),
		BotDisplayName:     "NoteTaker",
		JoinTimeoutS:       600,
		MaxDurationS:       14400,
		EmptyGraceS:        60,
		MinRecordingS:      15,
		AudioRetentionDays: 7,
		WebhookURL:         recv.URL,
		WebhookSecret:      e2eSecret,
		ListenAddr:         "127.0.0.1:0",
		PublicURL:          "http://localhost:8080",
		LogLevel:           "ERROR",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, func(a net.Addr) { addrs <- a }) }()

	var addr net.Addr
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the HTTP server never came up")
	}
	base := "http://" + addr.String()

	if resp, err := http.Get(base + "/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health: %v %v", err, resp)
	}

	var payload finishedPayload
	select {
	case body := <-delivered:
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("webhook body: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no recording.finished webhook arrived")
	}
	id := strconv.FormatInt(e2eMsgID, 10)
	switch {
	case payload.Event != eventRecordingFinished:
		t.Errorf("event = %q", payload.Event)
	case payload.ID != id || payload.MessageID != e2eMsgID:
		t.Errorf("id = %q/%d", payload.ID, payload.MessageID)
	case payload.Stream != e2eStream || payload.Topic != e2eTopic:
		t.Errorf("stream/topic = %q/%q", payload.Stream, payload.Topic)
	case payload.JitsiURL != e2eRoom:
		t.Errorf("jitsi_url = %q", payload.JitsiURL)
	case payload.DurationS != 120:
		t.Errorf("duration_s = %v", payload.DurationS)
	case payload.AudioPath != filepath.Join(jobDir(dataDir, id), "audio.webm"):
		t.Errorf("audio_path = %q", payload.AudioPath)
	case payload.CallbackURL != cfg.PublicURL+"/notify":
		t.Errorf("callback_url = %q", payload.CallbackURL)
	}

	// The job is finished on disk and stamped as delivered.
	var job Job
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		j, err := loadJob(dataDir, id)
		if err == nil && j.WebhookSentAt != nil {
			job = j
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.State != JobFinished || job.WebhookSentAt == nil {
		t.Fatalf("job.json = %+v, want finished with webhook_sent_at", job)
	}

	// 🎙️ offered, 🔴 added at the start of the recording and removed at the end.
	want := []string{"add:" + micEmoji, "add:" + recordingEmoji, "remove:" + recordingEmoji}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if len(zulip.reactionLog()) >= len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := zulip.reactionLog(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("reactions = %v, want %v", got, want)
	}

	// The callback: signed like the outbound webhook, posted into the same topic.
	notifyBody := []byte(`{"id":"` + id + `","content":"Transcript ready."}`)
	req, err := http.NewRequest(http.MethodPost, base+"/notify", bytes.NewReader(notifyBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(headerSignature, signBody(notifyBody, e2eSecret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /notify: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /notify: http %d", resp.StatusCode)
	}
	select {
	case m := <-zulip.msgs:
		if m.stream != e2eStream || m.topic != e2eTopic || m.content != "Transcript ready." {
			t.Errorf("notify posted %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notify posted nothing to Zulip")
	}

	// An unsigned callback is refused.
	resp2, err := http.Post(base+"/notify", "application/json", bytes.NewReader(notifyBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned /notify: http %d, want 401", resp2.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after the context ended")
	}
	if _, err := http.Get(base + "/health"); err == nil {
		t.Error("the HTTP server is still listening after shutdown")
	}

	// The operator-visible lifecycle, in order. "msg=recorder" is the fake
	// recorder's "fake recorder: joined" stderr line, promoted out of Debug.
	out := logs.String()
	for pos, want := 0, []string{
		`msg="call link detected"`,
		`msg="recording requested"`,
		`msg="recording started"`,
		`msg=recorder job=`,
		`msg="recorder finished"`,
		`msg="job finished"`,
		`msg="webhook sent"`,
	}; len(want) > 0; want = want[1:] {
		i := strings.Index(out[pos:], want[0])
		if i < 0 {
			t.Fatalf("INFO log has no %s after offset %d; log:\n%s", want[0], pos, out)
		}
		pos += i + len(want[0])
	}
}
