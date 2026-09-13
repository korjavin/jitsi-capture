package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// maxNotifyBody caps the inbound callback body: a transcript notification is a
// couple of lines, so a megabyte is already generous.
const maxNotifyBody = 1 << 20

// server serves the inbound HTTP surface: the transcript callback and a health
// probe. Its dependencies are function values so this file compiles without the
// runner and the Zulip client — the integration wiring supplies the real ones.
type server struct {
	secret  string
	loadJob func(id string) (Job, error)
	send    func(ctx context.Context, stream, topic, content string) error
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeOK(w) })
	mux.HandleFunc("POST /notify", s.notify)
	return mux
}

// notify posts a downstream service's message ("transcript ready", a link, …)
// into the Zulip topic the recording came from. The body is signed with the
// same secret as the outbound webhook.
func (s *server) notify(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxNotifyBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifySignature(body, r.Header.Get(headerSignature), s.secret) {
		slog.Warn("notify rejected: invalid signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	var req struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == "" || req.Content == "" {
		http.Error(w, "id and content are required", http.StatusBadRequest)
		return
	}
	job, err := s.loadJob(req.ID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		slog.Warn("notify: unknown job", "job", req.ID)
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	case err != nil:
		slog.Error("notify: loading job", "job", req.ID, "err", err)
		http.Error(w, "cannot load job", http.StatusInternalServerError)
		return
	}
	// The content itself is user-visible text, never logged at info level.
	if err := s.send(r.Context(), job.Stream, job.Topic, req.Content); err != nil {
		slog.Error("notify: posting to zulip", "job", req.ID, "err", err)
		http.Error(w, "cannot post to zulip", http.StatusBadGateway)
		return
	}
	slog.Info("notify delivered", "job", req.ID)
	writeOK(w)
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// newHTTPServer builds the listener. main runs ListenAndServe and calls
// Shutdown on SIGTERM.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
}
