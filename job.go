package main

import (
	"errors"
	"time"
)

// Job is the on-disk record of one recording, stored at
// DATA_DIR/jobs/<id>/job.json. ID is the Zulip message id the 🎙️ reaction
// landed on.
type Job struct {
	ID            string     `json:"id"`
	MessageID     int64      `json:"message_id"`
	Stream        string     `json:"stream"`
	Topic         string     `json:"topic"`
	JitsiURL      string     `json:"jitsi_url"` // room URL only — never tokens/passwords
	State         string     `json:"state"`     // recording | finished | failed
	Error         string     `json:"error,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	DurationS     float64    `json:"duration_s"`
	AudioPath     string     `json:"audio_path,omitempty"` // container path DATA_DIR/jobs/<id>/audio.webm
	Participants  []string   `json:"participants"`
	WebhookSentAt *time.Time `json:"webhook_sent_at,omitempty"`
	Tracks        []Track    `json:"tracks,omitempty"` // filled once the recorder emits per-participant audio
}

// Track is one participant's audio within a job.
type Track struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	OffsetS float64 `json:"offset_s"`
	EndedS  float64 `json:"ended_s"`
}

// Job.State values.
const (
	JobRecording = "recording"
	JobFinished  = "finished"
	JobFailed    = "failed"
)

// ErrDuplicateJob is returned when a recording is requested for a room that is
// already being recorded.
var ErrDuplicateJob = errors.New("job already recording")
