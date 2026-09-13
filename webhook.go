package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Headers of the outbound recording.finished webhook. The signature header is
// also what POST /notify verifies on the way back in.
const (
	headerEvent     = "x-jitsi-capture-event"
	headerSignature = "x-jitsi-capture-signature"

	eventRecordingFinished = "recording.finished"
)

// backoff is the retry schedule for a webhook the receiver did not accept.
// ponytail: fixed table; unsent jobs are retried hourly by the sweep anyway.
var backoff = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 2 * time.Minute, 5 * time.Minute}

// webhookClient is the client every delivery attempt uses. Redirects are not
// followed: net/http would turn a 30x into a GET without the signed body, and a
// 200 on that would look like a delivery that never happened.
var webhookClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// signBody is the HMAC-SHA256 of the raw body, in tr2outline's format:
// "sha256=" + lowercase hex.
func signBody(body []byte, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// verifySignature reports whether header is a valid signature of body. The
// "sha256=" prefix is optional. An empty secret or header is never valid — no
// secret means no trust.
func verifySignature(body []byte, header, secret string) bool {
	if header == "" || secret == "" {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// finishedPayload is the recording.finished body. Paths are host-visible, so
// the receiving service reads them through its own bind mount.
type finishedPayload struct {
	Event        string   `json:"event"`
	ID           string   `json:"id"`
	MessageID    int64    `json:"message_id"`
	Stream       string   `json:"stream"`
	Topic        string   `json:"topic"`
	JitsiURL     string   `json:"jitsi_url"`
	AudioPath    string   `json:"audio_path"`
	DurationS    float64  `json:"duration_s"`
	StartedAt    string   `json:"started_at"`
	EndedAt      string   `json:"ended_at"`
	Participants []string `json:"participants"`
	CallbackURL  string   `json:"callback_url"`
	Tracks       []Track  `json:"tracks,omitempty"`
}

// hostPath rebases a container path onto the host bind mount. filepath.Rel
// cleans both sides, so a trailing slash on DATA_DIR is harmless and a sibling
// such as /data-other is not mistaken for a child of /data; anything not under
// DataDir is passed through untouched.
func hostPath(cfg Config, p string) string {
	if p == "" || cfg.HostDataDir == cfg.DataDir {
		return p
	}
	rel, err := filepath.Rel(cfg.DataDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return filepath.Join(cfg.HostDataDir, rel)
}

func buildPayload(cfg Config, job Job) finishedPayload {
	p := finishedPayload{
		Event:        eventRecordingFinished,
		ID:           job.ID,
		MessageID:    job.MessageID,
		Stream:       job.Stream,
		Topic:        job.Topic,
		JitsiURL:     job.JitsiURL,
		AudioPath:    hostPath(cfg, job.AudioPath),
		DurationS:    job.DurationS,
		StartedAt:    job.StartedAt.Format(time.RFC3339),
		Participants: job.Participants,
		CallbackURL:  cfg.PublicURL + "/notify",
	}
	if p.Participants == nil {
		p.Participants = []string{} // never null: the receiver ranges over it
	}
	if job.EndedAt != nil {
		p.EndedAt = job.EndedAt.Format(time.RFC3339)
	}
	for _, t := range job.Tracks {
		t.Path = hostPath(cfg, t.Path)
		p.Tracks = append(p.Tracks, t)
	}
	return p
}

// sendWebhook delivers recording.finished, retrying on the backoff table. It
// returns nil when the receiver answered 2xx (or when no webhook is
// configured), and the last error once the table is exhausted.
func sendWebhook(ctx context.Context, cfg Config, job Job) error {
	if cfg.WebhookURL == "" {
		slog.Info("webhook disabled", "job", job.ID)
		return nil
	}
	body, err := json.Marshal(buildPayload(cfg, job))
	if err != nil {
		return err
	}
	sig := signBody(body, cfg.WebhookSecret)

	for attempt := 0; ; attempt++ {
		err := postWebhook(ctx, cfg.WebhookURL, body, sig)
		if err == nil {
			slog.Info("webhook sent", "job", job.ID, "attempt", attempt+1)
			return nil
		}
		if attempt >= len(backoff) {
			slog.Error("webhook giving up", "job", job.ID, "attempts", attempt+1, "err", err)
			return err
		}
		slog.Warn("webhook failed, retrying", "job", job.ID, "attempt", attempt+1, "in", backoff[attempt], "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff[attempt]):
		}
	}
}

// postWebhook is one delivery attempt. Any non-2xx status is an error carrying
// the status code — never the secret.
func postWebhook(ctx context.Context, url string, body []byte, sig string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerEvent, eventRecordingFinished)
	req.Header.Set(headerSignature, sig)

	resp, err := webhookClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: http %d", resp.StatusCode)
	}
	return nil
}
