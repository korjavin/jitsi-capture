package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The retry table is zeroed so the retry tests do not actually wait. Set in
// init, like nodeBin in runner_test.go, so no test goroutine races it.
func init() { backoff = []time.Duration{0, 0} }

// Fixed vector: printf '%s' '{"a":1}' | openssl dgst -sha256 -hmac test
const (
	vectorBody   = `{"a":1}`
	vectorSecret = "test"
	vectorSig    = "sha256=3b76df928ca0fb147722b51bd3e4e9f62e88b99eb9575cb0b2ded29e6e812402"
)

func TestSignBodyVector(t *testing.T) {
	if got := signBody([]byte(vectorBody), vectorSecret); got != vectorSig {
		t.Fatalf("signBody = %q, want %q", got, vectorSig)
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(vectorBody)
	bare := vectorSig[len("sha256="):]

	for _, tc := range []struct {
		name           string
		body           []byte
		header, secret string
		want           bool
	}{
		{"with prefix", body, vectorSig, vectorSecret, true},
		{"without prefix", body, bare, vectorSecret, true},
		{"tampered body", []byte(`{"a":2}`), vectorSig, vectorSecret, false},
		{"wrong secret", body, vectorSig, "other", false},
		{"empty header", body, "", vectorSecret, false},
		{"empty secret", body, vectorSig, "", false},
		{"not hex", body, "sha256=zz", vectorSecret, false},
	} {
		if got := verifySignature(tc.body, tc.header, tc.secret); got != tc.want {
			t.Errorf("%s: verifySignature = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// finishedJob is a finished job with both an audio path and a track under DATA_DIR.
func finishedJob() Job {
	started := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	return Job{
		ID:           "42",
		MessageID:    42,
		Stream:       "stream-x",
		Topic:        "topic-y",
		JitsiURL:     "https://meet.example.com/room-z",
		State:        JobFinished,
		StartedAt:    started,
		EndedAt:      &ended,
		DurationS:    90,
		AudioPath:    "/data/jobs/42/audio.webm",
		Participants: []string{"Alice", "Bob"},
		Tracks:       []Track{{ID: "t1", Name: "Alice", Path: "/data/jobs/42/t1.webm", OffsetS: 1, EndedS: 89}},
	}
}

func webhookConfig(webhookURL string) Config {
	return Config{
		DataDir:       "/data",
		HostDataDir:   "/srv/capture",
		WebhookURL:    webhookURL,
		WebhookSecret: vectorSecret,
		PublicURL:     "http://localhost:8080",
	}
}

func TestSendWebhookPayloadAndHeaders(t *testing.T) {
	type received struct {
		event, sig string
		body       []byte
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		got <- received{r.Header.Get(headerEvent), r.Header.Get(headerSignature), b}
	}))
	defer srv.Close()

	cfg := webhookConfig(srv.URL)
	if err := sendWebhook(context.Background(), cfg, finishedJob()); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
	r := <-got

	if r.event != eventRecordingFinished {
		t.Errorf("event header = %q", r.event)
	}
	if !verifySignature(r.body, r.sig, vectorSecret) {
		t.Errorf("signature does not verify against the raw body")
	}

	var p finishedPayload
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	want := finishedPayload{
		Event:        eventRecordingFinished,
		ID:           "42",
		MessageID:    42,
		Stream:       "stream-x",
		Topic:        "topic-y",
		JitsiURL:     "https://meet.example.com/room-z",
		AudioPath:    "/srv/capture/jobs/42/audio.webm",
		DurationS:    90,
		StartedAt:    "2026-09-13T10:00:00Z",
		EndedAt:      "2026-09-13T10:01:30Z",
		Participants: []string{"Alice", "Bob"},
		CallbackURL:  "http://localhost:8080/notify",
		Tracks:       []Track{{ID: "t1", Name: "Alice", Path: "/srv/capture/jobs/42/t1.webm", OffsetS: 1, EndedS: 89}},
	}
	if p.AudioPath != want.AudioPath || len(p.Tracks) != 1 || p.Tracks[0] != want.Tracks[0] {
		t.Errorf("host paths not rebased: audio %q tracks %+v", p.AudioPath, p.Tracks)
	}
	if p.Event != want.Event || p.ID != want.ID || p.MessageID != want.MessageID ||
		p.Stream != want.Stream || p.Topic != want.Topic || p.JitsiURL != want.JitsiURL ||
		p.DurationS != want.DurationS || p.StartedAt != want.StartedAt || p.EndedAt != want.EndedAt ||
		p.CallbackURL != want.CallbackURL {
		t.Errorf("payload = %+v, want %+v", p, want)
	}
}

func TestSendWebhookParticipantsNeverNull(t *testing.T) {
	job := finishedJob()
	job.Participants = nil
	job.Tracks = nil

	b, err := json.Marshal(buildPayload(webhookConfig("http://example.invalid"), job))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(raw["participants"]) != "[]" {
		t.Errorf("participants = %s, want []", raw["participants"])
	}
	if _, ok := raw["tracks"]; ok {
		t.Errorf("tracks should be omitted when the recorder emitted none")
	}
}

func TestSendWebhookRetries(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := sendWebhook(context.Background(), webhookConfig(srv.URL), finishedJob()); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestSendWebhookGivesUp(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := sendWebhook(context.Background(), webhookConfig(srv.URL), finishedJob())
	if err == nil {
		t.Fatal("sendWebhook: want an error once the backoff table is exhausted")
	}
	if got, want := int(calls.Load()), len(backoff)+1; got != want {
		t.Errorf("calls = %d, want %d", got, want)
	}
}

// A redirect must not be followed: net/http would drop the signed body and a
// 200 at the end of the hop would hide a failed delivery.
func TestSendWebhookDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "/login", http.StatusFound)
	}))
	defer srv.Close()

	if err := sendWebhook(context.Background(), webhookConfig(srv.URL), finishedJob()); err == nil {
		t.Fatal("sendWebhook: a redirect must be reported as a failed delivery")
	}
}

func TestSendWebhookDisabled(t *testing.T) {
	// An empty WebhookURL must not even be dialled; a bad URL would error out.
	cfg := webhookConfig("")
	if err := sendWebhook(context.Background(), cfg, finishedJob()); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}
}

func TestHostPath(t *testing.T) {
	cfg := webhookConfig("")
	trailing := cfg
	trailing.DataDir = "/data/"

	for _, tc := range []struct {
		name     string
		cfg      Config
		in, want string
	}{
		{"under data dir", cfg, "/data/jobs/42/audio.webm", "/srv/capture/jobs/42/audio.webm"},
		{"trailing slash on data dir", trailing, "/data/jobs/42/audio.webm", "/srv/capture/jobs/42/audio.webm"},
		{"sibling directory", cfg, "/data-other/a.webm", "/data-other/a.webm"},
		{"foreign path", cfg, "/elsewhere/a.webm", "/elsewhere/a.webm"},
		{"empty", cfg, "", ""},
	} {
		if got := hostPath(tc.cfg, tc.in); got != tc.want {
			t.Errorf("%s: hostPath(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
