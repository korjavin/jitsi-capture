package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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

// jobDir is the per-job directory: DATA_DIR/jobs/<id>.
func jobDir(dataDir, id string) string {
	return filepath.Join(dataDir, "jobs", id)
}

// save writes job.json atomically: a temp file in the same directory followed by
// a rename, so a crash mid-write never leaves a half-written job.json behind.
func (j *Job) save(dataDir string) error {
	dir := jobDir(dataDir, j.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "job-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename below succeeded
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "job.json"))
}

// loadJob reads one job. A missing job wraps os.ErrNotExist, which the HTTP
// layer maps to 404.
func loadJob(dataDir, id string) (Job, error) {
	var j Job
	b, err := os.ReadFile(filepath.Join(jobDir(dataDir, id), "job.json"))
	if err != nil {
		return j, err
	}
	return j, json.Unmarshal(b, &j)
}

// listJobs returns every readable job under DATA_DIR/jobs. Unreadable ones are
// logged and skipped rather than failing the whole listing — one corrupt
// job.json must not block startup recovery or the retention sweep.
func listJobs(dataDir string) ([]Job, error) {
	ents, err := os.ReadDir(filepath.Join(dataDir, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []Job
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		j, err := loadJob(dataDir, e.Name())
		if err != nil {
			slog.Warn("skipping unreadable job", "id", e.Name(), "err", err)
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}
