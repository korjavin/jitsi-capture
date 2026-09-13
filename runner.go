package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// zulipAPI is the slice of the Zulip client the runner needs. *Zulip satisfies
// it; tests use a fake.
type zulipAPI interface {
	RemoveReaction(ctx context.Context, msgID int64, emoji string) error
	SendMessage(ctx context.Context, stream, topic, content string) error
}

// Runner owns the recorder child processes and the job state on disk.
type Runner struct {
	cfg        Config
	z          zulipAPI
	onFinished func(Job) // set by the integration wiring; sends the webhook

	mu       sync.Mutex
	stopping bool
	running  map[string]*recording
}

// recording tracks one accepted job from Start until its final state is on
// disk — not merely until the child exits, so Stop can neither miss a job whose
// child has not spawned yet nor return while a job.json still says "recording".
type recording struct {
	cmd    *exec.Cmd     // nil until the child has actually been started
	termed bool          // SIGTERM already sent; a second one skips finalization
	done   chan struct{} // closed once the job has been settled
}

// nodeBin is the interpreter the recorder is launched with.
// ponytail: a package var so tests can set it to "sh" and let testdata/*.sh
// stand in for record.js — no interface, no injection.
var nodeBin = "node"

// Job.Error values.
const (
	errNotAdmitted    = "not_admitted"
	errRecorderFailed = "recorder_failed"
	errTooShort       = "too_short"
	errInterrupted    = "interrupted"
)

// recordingEmoji is the reaction the bot adds while recording and removes when
// the job ends, whatever the outcome.
const recordingEmoji = "red_circle"

// failNote maps a Job.Error to the single English line posted in the job's topic.
var failNote = map[string]string{
	errNotAdmitted:    "NoteTaker was not admitted to the call (or nobody joined) — nothing recorded.",
	errRecorderFailed: "Recording failed (recorder error) — nothing recorded.",
	errTooShort:       "Recording too short (under 15 s) — nothing to transcribe.",
	errInterrupted:    "Recording was interrupted by a service restart — no transcript.",
}

const (
	stderrTailLines = 20               // recorder stderr kept for the failure log
	zulipTimeout    = 15 * time.Second // budget for the reaction/message calls
)

func newRunner(cfg Config, z zulipAPI, onFinished func(Job)) *Runner {
	return &Runner{cfg: cfg, z: z, onFinished: onFinished, running: map[string]*recording{}}
}

// Start records the job as recording and spawns the recorder. It returns
// ErrDuplicateJob when a recording for the same message is already in flight; a
// previously failed or finished job may be re-recorded, reusing its directory.
func (r *Runner) Start(job Job) error {
	r.mu.Lock()
	defer r.mu.Unlock() // held across check+save so two Starts cannot both win

	if old, err := loadJob(r.cfg.DataDir, job.ID); err == nil && old.State == JobRecording {
		return ErrDuplicateJob
	}
	job.State = JobRecording
	job.Error = ""
	job.StartedAt = time.Now()
	job.EndedAt = nil
	job.DurationS = 0
	job.WebhookSentAt = nil
	job.AudioPath = filepath.Join(jobDir(r.cfg.DataDir, job.ID), "audio.webm")
	if err := job.save(r.cfg.DataDir); err != nil {
		return err
	}
	// Registered here, not in run: Stop must see a job it just accepted.
	rec := &recording{done: make(chan struct{})}
	r.running[job.ID] = rec
	go r.run(job, rec)
	return nil
}

// run drives one recorder child process to completion and settles the job.
func (r *Runner) run(job Job, rec *recording) {
	defer func() {
		r.mu.Lock()
		if r.running[job.ID] == rec { // a re-record may already have replaced it
			delete(r.running, job.ID)
		}
		r.mu.Unlock()
		close(rec.done)
	}()

	cmd := exec.Command(nodeBin, r.cfg.RecorderPath,
		"--url", job.JitsiURL,
		"--out", job.AudioPath,
		// Per-participant audio next to the mixed file; the recorder reports the
		// files it wrote back in the stdout JSON as tracks[].
		"--tracks-dir", filepath.Join(jobDir(r.cfg.DataDir, job.ID), "tracks"),
		"--join-timeout", strconv.Itoa(r.cfg.JoinTimeoutS),
		"--max-duration", strconv.Itoa(r.cfg.MaxDurationS),
		"--empty-grace", strconv.Itoa(r.cfg.EmptyGraceS),
		"--display-name", r.cfg.BotDisplayName,
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	var tail []string
	stderr, err := cmd.StderrPipe()
	if err == nil {
		if err = cmd.Start(); err == nil {
			r.mu.Lock()
			rec.cmd = cmd
			if r.stopping {
				// Stop ran between Start and here, so this child missed the
				// signal round; send it now, still under the lock so it cannot
				// also be signalled by Stop.
				r.termLocked(job.ID, rec)
			}
			r.mu.Unlock()

			tail = drainStderr(stderr, job.ID)
			err = cmd.Wait()
		}
	}

	now := time.Now()
	job.EndedAt = &now
	code := exitCode(err)
	switch {
	case code == 3:
		failJob(&job, errNotAdmitted, code, tail)
	case code != 0:
		failJob(&job, errRecorderFailed, code, tail)
	default:
		res, perr := parseResult(stdout.Bytes())
		switch {
		case perr != nil:
			slog.Error("recorder output unparsable", "job", job.ID, "err", perr)
			failJob(&job, errRecorderFailed, code, tail)
		case res.DurationS < float64(r.cfg.MinRecordingS):
			failJob(&job, errTooShort, code, tail)
		default:
			job.State = JobFinished
			job.DurationS = res.DurationS
			job.Participants = res.Participants
			job.Tracks = res.Tracks
		}
	}
	r.settle(job)
}

// settle posts the failure note if any, drops the recording reaction, persists
// the job and hands a finished job to the webhook sender.
func (r *Runner) settle(job Job) {
	ctx, cancel := context.WithTimeout(context.Background(), zulipTimeout)
	defer cancel()

	if job.State == JobFailed {
		r.note(ctx, job)
	}
	r.clearReaction(ctx, job)
	if err := job.save(r.cfg.DataDir); err != nil {
		slog.Error("saving job", "job", job.ID, "err", err)
	}
	if job.State == JobFinished && r.onFinished != nil {
		r.onFinished(job)
	}
}

func (r *Runner) note(ctx context.Context, job Job) {
	text, ok := failNote[job.Error]
	if !ok {
		return
	}
	if err := r.z.SendMessage(ctx, job.Stream, job.Topic, text); err != nil {
		slog.Error("posting failure note", "job", job.ID, "err", err)
	}
}

func (r *Runner) clearReaction(ctx context.Context, job Job) {
	if err := r.z.RemoveReaction(ctx, job.MessageID, recordingEmoji); err != nil {
		slog.Error("removing reaction", "job", job.ID, "err", err)
	}
}

// Stop asks every running recorder to finalize (SIGTERM, which makes record.js
// exit 0 with reason "signal") and returns once every accepted job has its
// final state on disk, or once grace has elapsed — then it kills the stragglers.
func (r *Runner) Stop(grace time.Duration) {
	r.mu.Lock()
	r.stopping = true
	recs := make([]*recording, 0, len(r.running))
	for _, rec := range r.running {
		recs = append(recs, rec)
	}
	r.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	slog.Info("stopping recorders", "count", len(recs), "grace", grace)
	r.termRunning()

	deadline := time.Now().Add(grace)
	for _, rec := range recs {
		select {
		case <-rec.done:
		case <-time.After(time.Until(deadline)):
			slog.Warn("recorders did not stop within grace, killing")
			// ponytail: killed children are left to settle on their own —
			// blocking shutdown past the grace period is the worse failure.
			r.killRunning()
			return
		}
	}
}

// termRunning asks every started child to finalize, at most once each: record.js
// exits immediately on a second signal, skipping finalization and its JSON line.
func (r *Runner) termRunning() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, rec := range r.running {
		r.termLocked(id, rec)
	}
}

// termLocked must be called with r.mu held.
func (r *Runner) termLocked(id string, rec *recording) {
	if rec.termed || rec.cmd == nil {
		return
	}
	rec.termed = true
	signalProcess(id, rec.cmd, syscall.SIGTERM)
}

func (r *Runner) killRunning() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, rec := range r.running {
		signalProcess(id, rec.cmd, syscall.SIGKILL)
	}
}

func signalProcess(id string, cmd *exec.Cmd, sig os.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(sig); err != nil {
		slog.Debug("signalling recorder", "job", id, "sig", sig, "err", err) // usually "already finished"
	}
}

// Resume repairs state left behind by a restart: a recording cannot survive the
// container (the browser died with it), so it is marked interrupted; a finished
// job whose webhook never went out is handed back to the sender.
func (r *Runner) Resume(ctx context.Context) {
	jobs, err := listJobs(r.cfg.DataDir)
	if err != nil {
		slog.Error("listing jobs on resume", "err", err)
		return
	}
	for _, job := range jobs {
		switch {
		case job.State == JobRecording:
			now := time.Now()
			job.State = JobFailed
			job.Error = errInterrupted
			job.EndedAt = &now
			slog.Warn("job interrupted by restart", "job", job.ID)
			r.note(ctx, job)
			r.clearReaction(ctx, job)
			if err := job.save(r.cfg.DataDir); err != nil {
				slog.Error("saving job", "job", job.ID, "err", err)
			}
		case job.State == JobFinished && job.WebhookSentAt == nil:
			slog.Info("retrying webhook after restart", "job", job.ID)
			if r.onFinished != nil {
				r.onFinished(job)
			}
		}
	}
}

// sweepRetention removes job directories older than days. A job still recording
// is never touched.
func sweepRetention(dataDir string, days int) {
	if days <= 0 {
		return // ponytail: non-positive retention means keep everything
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	ents, err := os.ReadDir(filepath.Join(dataDir, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Error("retention sweep", "err", err)
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		var ts time.Time
		if job, err := loadJob(dataDir, id); err == nil {
			if job.State == JobRecording {
				continue
			}
			if job.EndedAt != nil {
				ts = *job.EndedAt
			}
		}
		if ts.IsZero() { // unreadable or never ended: fall back to the directory
			fi, err := e.Info()
			if err != nil {
				continue
			}
			ts = fi.ModTime()
		}
		if ts.After(cutoff) {
			continue
		}
		if err := os.RemoveAll(jobDir(dataDir, id)); err != nil {
			slog.Error("removing expired job", "job", id, "err", err)
			continue
		}
		slog.Info("removed expired job", "job", id, "age_cutoff", cutoff)
	}
}

// recResult is the recorder's final stdout line. participants and tracks are
// optional — tracks only appears once the per-track recorder lands.
type recResult struct {
	Out          string   `json:"out"`
	DurationS    float64  `json:"duration_s"`
	Reason       string   `json:"reason"`
	Participants []string `json:"participants"`
	Tracks       []Track  `json:"tracks"`
}

// parseResult reads the last non-empty line of the recorder's stdout as JSON.
func parseResult(stdout []byte) (recResult, error) {
	var res recResult
	lines := strings.Split(string(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		return res, json.Unmarshal([]byte(line), &res)
	}
	return res, errors.New("recorder printed no output")
}

// drainStderr logs every recorder stderr line at debug and keeps the last few
// for the failure log.
func drainStderr(r io.Reader, id string) []string {
	var tail []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		slog.Debug("recorder", "job", id, "line", line)
		tail = append(tail, line)
		if len(tail) > stderrTailLines {
			tail = tail[1:]
		}
	}
	return tail
}

func failJob(job *Job, reason string, code int, tail []string) {
	job.State = JobFailed
	job.Error = reason
	slog.Error("recording failed", "job", job.ID, "reason", reason,
		"exit_code", code, "stderr", strings.Join(tail, "\n"))
}

// exitCode is the child's exit status; -1 when it could not be run at all.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
