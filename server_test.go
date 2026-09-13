package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type sentMessage struct {
	stream, topic, content string
}

// newTestServer returns a handler backed by one known job ("42") plus a
// channel that receives whatever the handler asked Zulip to post.
func newTestServer(secret string, sendErr error) (http.Handler, chan sentMessage) {
	sent := make(chan sentMessage, 1)
	s := &server{
		secret: secret,
		loadJob: func(id string) (Job, error) {
			if id != "42" {
				return Job{}, os.ErrNotExist
			}
			return Job{ID: "42", Stream: "stream-x", Topic: "topic-y"}, nil
		},
		send: func(_ context.Context, stream, topic, content string) error {
			if sendErr != nil {
				return sendErr
			}
			sent <- sentMessage{stream, topic, content}
			return nil
		},
	}
	return s.handler(), sent
}

// notify posts body to /notify, signing it with sigSecret ("" = no header).
func notify(h http.Handler, body, sigSecret string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	if sigSecret != "" {
		r.Header.Set(headerSignature, signBody([]byte(body), sigSecret))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHealth(t *testing.T) {
	h, _ := newTestServer(vectorSecret, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %q", got)
	}
}

func TestNotifyOK(t *testing.T) {
	h, sent := newTestServer(vectorSecret, nil)
	w := notify(h, `{"id":"42","content":"Transcript ready"}`, vectorSecret)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	got := <-sent
	want := sentMessage{"stream-x", "topic-y", "Transcript ready"}
	if got != want {
		t.Errorf("sent = %+v, want %+v", got, want)
	}
}

func TestNotifyRejections(t *testing.T) {
	for _, tc := range []struct {
		name, body, sigSecret string
		want                  int
	}{
		{"missing signature", `{"id":"42","content":"x"}`, "", http.StatusUnauthorized},
		{"wrong signature", `{"id":"42","content":"x"}`, "other-secret", http.StatusUnauthorized},
		{"malformed json", `{"id":`, vectorSecret, http.StatusBadRequest},
		{"empty id", `{"id":"","content":"x"}`, vectorSecret, http.StatusBadRequest},
		{"empty content", `{"id":"42","content":""}`, vectorSecret, http.StatusBadRequest},
		{"unknown id", `{"id":"99","content":"x"}`, vectorSecret, http.StatusNotFound},
	} {
		h, _ := newTestServer(vectorSecret, nil)
		if w := notify(h, tc.body, tc.sigSecret); w.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}

// A tampered body must fail even though the signature itself is well-formed.
func TestNotifyTamperedBody(t *testing.T) {
	h, _ := newTestServer(vectorSecret, nil)
	r := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(`{"id":"42","content":"evil"}`))
	r.Header.Set(headerSignature, signBody([]byte(`{"id":"42","content":"x"}`), vectorSecret))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// No secret configured means no trust: a correctly signed body is still refused.
func TestNotifyWithoutSecret(t *testing.T) {
	h, _ := newTestServer("", nil)
	body := `{"id":"42","content":"x"}`
	if w := notify(h, body, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("unsigned: status = %d, want 401", w.Code)
	}
	if w := notify(h, body, vectorSecret); w.Code != http.StatusUnauthorized {
		t.Errorf("signed: status = %d, want 401", w.Code)
	}
}

func TestNotifySendFailure(t *testing.T) {
	h, _ := newTestServer(vectorSecret, os.ErrPermission)
	if w := notify(h, `{"id":"42","content":"x"}`, vectorSecret); w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestNewHTTPServer(t *testing.T) {
	h, _ := newTestServer(vectorSecret, nil)
	srv := newHTTPServer(":8080", h)
	if srv.Addr != ":8080" || srv.ReadHeaderTimeout == 0 || srv.Handler == nil {
		t.Errorf("server = %+v", srv)
	}
}
