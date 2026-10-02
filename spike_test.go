package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSpike(t *testing.T) {
	for _, tc := range []struct {
		in   string
		url  string
		secs int
		ok   bool
	}{
		{"meet-spike https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij", 90, true},
		{"  meet-spike <https://meet.google.com/abc-defg-hij> seconds=30 ", "https://meet.google.com/abc-defg-hij", 30, true},
		{"meet-spike https://meet.google.com/abc-defg-hij seconds=9999", "https://meet.google.com/abc-defg-hij", 600, true},
		{"meet-spike https://meet.google.com/abc-defg-hij seconds=0", "", 0, false},
		{"meet-spike https://meet.google.com/abc-defg-hij?authuser=1", "", 0, false},
		{"meet-spike https://evil.example/abc", "", 0, false},
		{"meet-spike", "", 0, false},
	} {
		u, s, ok := parseSpike(tc.in)
		if u != tc.url || s != tc.secs || ok != tc.ok {
			t.Errorf("parseSpike(%q) = %q, %d, %v; want %q, %d, %v", tc.in, u, s, ok, tc.url, tc.secs, tc.ok)
		}
	}
}

// spikeFixture runs Spike against an httptest Zulip that accepts uploads.
func spikeFixture(t *testing.T) (*Spike, *zulipServer) {
	t.Helper()
	z, srv := newZulipServer(t, func(w http.ResponseWriter, r *http.Request, _ url.Values) {
		if r.URL.Path == "/api/v1/user_uploads" {
			ok(w, `{"result":"success","url":"/user_uploads/1/ab/meet-spike.webm"}`)
			return
		}
		ok(w, "")
	})
	s := newSpike(context.Background(), context.Background(),
		Config{DataDir: t.TempDir(), RecorderPath: "unused", BotDisplayName: "NoteTaker"}, z)
	s.script = filepath.Join("testdata", "meet_spike.sh")
	return s, srv
}

func dmReplies(srv *zulipServer) []string {
	var out []string
	for _, r := range srv.requests() {
		if r.path == "/api/v1/messages" && r.form.Get("type") == "private" {
			out = append(out, r.form.Get("content"))
		}
	}
	return out
}

func TestSpikeRunsAndReportsBack(t *testing.T) {
	s, srv := spikeFixture(t)
	s.Handle(Message{ID: 1, Type: "private", SenderID: 42, Content: "meet-spike https://meet.google.com/abc-defg-hij seconds=30"})
	s.Wait(15 * time.Second)

	replies := dmReplies(srv)
	if len(replies) != 2 || !strings.Contains(replies[0], "admit me from the lobby") {
		t.Fatalf("replies = %q; want the lobby notice then the report", replies)
	}
	for _, r := range srv.requests() {
		if r.path == "/api/v1/messages" && r.form.Get("to") != "[42]" {
			t.Errorf("reply went to %q; want the sender [42]", r.form.Get("to"))
		}
	}
	report := replies[1]
	for _, want := range []string{"exit code 0", "STATE - -> prejoin", "STATE prejoin -> admitted", `"liveAudioTracks":4`, "names", "SUMMARY",
		"[meet-spike.webm](/user_uploads/1/ab/meet-spike.webm)"} {
		if !strings.Contains(report, want) {
			t.Errorf("report misses %q:\n%s", want, report)
		}
	}
	for _, unwanted := range []string{`"liveAudioTracks":1`, "noise", "launch"} {
		if strings.Contains(report, unwanted) {
			t.Errorf("report should not carry %q:\n%s", unwanted, report)
		}
	}

	dirs, _ := filepath.Glob(filepath.Join(s.dataDir, "spike", "*", "args"))
	if len(dirs) != 1 {
		t.Fatalf("args files = %v; want one run under DATA_DIR/spike", dirs)
	}
	args, _ := os.ReadFile(dirs[0])
	want := "https://meet.google.com/abc-defg-hij --seconds 30 --join-timeout 300 --captions --name NoteTaker --out-dir " + filepath.Dir(dirs[0])
	if strings.TrimSpace(string(args)) != want {
		t.Errorf("script args = %q; want %q", args, want)
	}
}

func TestSpikeOneRunAtATime(t *testing.T) {
	t.Setenv("FAKE_SPIKE_SLEEP", "1")
	s, srv := spikeFixture(t)
	m := Message{ID: 1, Type: "private", SenderID: 42, Content: "meet-spike https://meet.google.com/abc-defg-hij"}
	s.Handle(m)
	s.Handle(m)
	s.Handle(Message{ID: 2, Type: "private", SenderID: 42, Content: "meet-spike please"})
	s.Wait(15 * time.Second)
	var busy, usage int
	for _, r := range dmReplies(srv) {
		if strings.HasPrefix(r, "busy") {
			busy++
		}
		if strings.HasPrefix(r, "usage") {
			usage++
		}
	}
	if busy != 1 || usage != 1 {
		t.Errorf("busy/usage replies = %d/%d; want 1/1: %q", busy, usage, dmReplies(srv))
	}
}

func TestBotRoutesSpikeDMs(t *testing.T) {
	f := newBotFixture(t, "https://meet.jit.si", Message{}, nil)
	var got []Message
	f.bot.spike = func(m Message) { got = append(got, m) }
	dm := Message{ID: 1, Type: "private", SenderID: 42, Content: "meet-spike https://meet.google.com/abc-defg-hij"}
	f.bot.handle(context.Background(), Event{Type: "message", Message: &dm})
	stream := Message{ID: 2, Type: "stream", SenderID: 42, Content: dm.Content}
	f.bot.handle(context.Background(), Event{Type: "message", Message: &stream})
	own := Message{ID: 3, Type: "private", SenderID: testBotID, Content: dm.Content}
	f.bot.handle(context.Background(), Event{Type: "message", Message: &own})
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("spike got %v; want only the DM from a user", got)
	}
}

func TestZulipUpload(t *testing.T) {
	var name, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, h, err := r.FormFile("file")
		if err != nil || r.URL.Path != "/api/v1/user_uploads" {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(f)
		name, body = h.Filename, string(b)
		ok(w, `{"result":"success","uri":"/user_uploads/1/x/a.webm"}`)
	}))
	defer srv.Close()
	z := newZulip(Config{ZulipSite: srv.URL, ZulipBotEmail: testBotEmail, ZulipBotAPIKey: testAPIKey})
	link, err := z.Upload(context.Background(), "a.webm", strings.NewReader("audio"))
	if err != nil || link != "/user_uploads/1/x/a.webm" || name != "a.webm" || body != "audio" {
		t.Errorf("Upload = %q, %v (server saw %q, %q); want the uri link and the file", link, err, name, body)
	}
}
