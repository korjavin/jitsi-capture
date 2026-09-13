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
	AddReaction(ctx context.Context, msgID int64, emoji string) error
	RemoveReaction(ctx context.Context, msgID int64, emoji string) error
	SendMessage(ctx context.Context, stream, topic, content string) error
}

// Runner owns the recorder child processes and the job state on disk.
type Runner struct {
	cfg        Config
	z          zulipAPI
	onFinished func(Job) // set by the integration wiring; sends the webhook
	// ctx bounds the Zulip calls the runner makes on its own goroutines. It
	// deliberately outlives SIGTERM — a recording finalized by the signal still
	// has to post its note and drop its 🔴 — and is cancelled once the service
	// is done waiting for them.
	ctx context.Context

	// mu covers the job-state transitions and nothing else: Start's
	// check-and-save, settle's final save and stampDelivered's
	// read-modify-write. They are the transitions that would otherwise clobber
	// each other on disk. No network call is ever made under it, so a slow
	// Zulip can delay neither a transition nor a shutdown.
	mu       sync.Mutex
	stopping bool
	running  map[string]*recording
	// indicators serializes the indicator calls per message; see lockIndicator.
	indicators map[int64]*indicator
}

// indicator is one message's indicator lock. waiting counts the callers holding
// or queued on it, so the map entry lives exactly as long as it is needed.
type indicator struct {
	mu      sync.Mutex
	waiting int
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

// recordingEmoji is the "recording now" indicator; the runner keeps it in step
// with job.json (see syncIndicator).
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

func newRunner(ctx context.Context, cfg Config, z zulipAPI, onFinished func(Job)) *Runner {
	return &Runner{
		ctx: ctx, cfg: cfg, z: z, onFinished: onFinished,
		running:    map[string]*recording{},
		indicators: map[int64]*indicator{},
	}
}

// Start records the job as recording and spawns the recorder, whose goroutine
// puts the recording indicator on the message before it joins. It returns
// ErrDuplicateJob when a recording for the same message is already in flight —
// re-asserting the indicator on the way out, so a click after an add that
// failed transiently still produces one. A previously failed or finished job
// may be re-recorded, reusing its directory.
//
// It talks to nothing but the disk, so the event loop calling it never waits on
// Zulip.
func (r *Runner) Start(job Job) error {
	rec, err := r.admit(&job)
	if err != nil {
		if errors.Is(err, ErrDuplicateJob) {
			// Warn: Zulip rejects an add for a reaction that is already there,
			// which is what this usually finds.
			go r.syncIndicator(job, slog.LevelWarn)
		}
		return err
	}
	go r.run(job, rec)
	return nil
}

// admit is Start's critical section: the duplicate check and the first save,
// atomic against every other job-state transition. It makes no network call, so
// nothing waiting on r.mu ever waits on Zulip.
func (r *Runner) admit(job *Job) (*recording, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if old, err := loadJob(r.cfg.DataDir, job.ID); err == nil && old.State == JobRecording {
		return nil, ErrDuplicateJob
	}
	job.State = JobRecording
	job.Error = ""
	job.StartedAt = time.Now()
	job.EndedAt = nil
	job.DurationS = 0
	job.WebhookSentAt = nil
	job.AudioPath = filepath.Join(jobDir(r.cfg.DataDir, job.ID), "audio.webm")
	if err := job.save(r.cfg.DataDir); err != nil {
		return nil, err
	}
	// Registered here, not in run: Stop must see a job it just accepted.
	rec := &recording{done: make(chan struct{})}
	r.running[job.ID] = rec
	return rec, nil
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

	// The indicator goes on here rather than in Start because it is a network
	// call: the event loop must not wait for Zulip, while this goroutine is
	// already covered by Stop's grace through rec.done. It is still on before
	// the child spawns, so a recorder that dies at once finds it there to clear.
	r.syncIndicator(job, slog.LevelError)

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

// settle persists the job and announces the outcome, then hands a finished job
// to the webhook sender.
func (r *Runner) settle(job Job) {
	err := r.saveFinalState(job)
	r.announce(job, err)

	if job.State == JobFinished && r.onFinished != nil {
		r.onFinished(job)
	}
}

// saveFinalState writes the settled job under the mutex Start's admission takes,
// because a settlement landing after a re-record's admission would otherwise
// resurrect "finished" over a live recording. It runs before any network call:
// Stop is waiting for exactly this file, and a click racing the settlement has
// to be able to read it.
func (r *Runner) saveFinalState(job Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := job.save(r.cfg.DataDir); err != nil {
		slog.Error("saving job", "job", job.ID, "err", err)
		return err
	}
	return nil
}

// announce posts the failure note if any and brings the indicator in line with
// the settled state. Both are Zulip calls, so a caller that must not block —
// Resume, on the startup path — runs it on a goroutine.
//
// saveErr is what saveFinalState reported: when the save failed, job.json still
// says "recording", so reading it back would put the indicator on a job that
// has already ended. The in-memory state is the only truth left.
func (r *Runner) announce(job Job, saveErr error) {
	// After the save, never before: whoever reads the note and clicks again
	// must find a job that is no longer recording, not a silent no-op.
	if job.State == JobFailed {
		r.note(job)
	}
	if saveErr != nil {
		r.clearIndicator(job)
		return
	}
	r.syncIndicator(job, slog.LevelError)
}

func (r *Runner) note(job Job) {
	text, ok := failNote[job.Error]
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, zulipTimeout)
	defer cancel()
	if err := r.z.SendMessage(ctx, job.Stream, job.Topic, text); err != nil {
		slog.Error("posting failure note", "job", job.ID, "err", err)
	}
}

// syncIndicator makes the indicator on the job's message match the job state on
// disk — the same file Start's duplicate check reads.
//
// Reading that state back, instead of carrying "add" or "remove" down from the
// caller, is what makes concurrent updates safe without holding r.mu across the
// network: the message's indicator lock serializes the calls, so whichever one
// runs last read a state at least as fresh as every save before it, and leaves
// the message matching that state. A click racing a settlement therefore cannot
// lose its indicator to the removal, and a settlement with no click behind it
// cannot strand one. The price is an occasional redundant call inside that
// window, which Zulip rejects harmlessly.
//
// addFailure is the level a failed add is logged at: the re-assert a duplicate
// click makes is expected to fail, an add on a fresh recording is not.
func (r *Runner) syncIndicator(job Job, addFailure slog.Level) {
	unlock := r.lockIndicator(job.MessageID)
	defer unlock()

	cur, err := loadJob(r.cfg.DataDir, job.ID)
	if err != nil {
		slog.Error("reading job state for the recording indicator", "job", job.ID, "err", err)
		return
	}
	if cur.State == JobRecording {
		r.callIndicator(r.z.AddReaction, "adding", job, addFailure)
		return
	}
	r.callIndicator(r.z.RemoveReaction, "removing", job, slog.LevelError)
}

// clearIndicator takes the indicator off without consulting job.json, for the
// one caller that knows better than the file: a settlement whose save failed.
func (r *Runner) clearIndicator(job Job) {
	unlock := r.lockIndicator(job.MessageID)
	defer unlock()
	r.callIndicator(r.z.RemoveReaction, "removing", job, slog.LevelError)
}

func (r *Runner) callIndicator(call func(context.Context, int64, string) error, verb string, job Job, failure slog.Level) {
	ctx, cancel := context.WithTimeout(r.ctx, zulipTimeout)
	defer cancel()
	if err := call(ctx, job.MessageID, recordingEmoji); err != nil {
		slog.Log(ctx, failure, verb+" the recording reaction", "job", job.ID, "err", err)
	}
}

// lockIndicator serializes the indicator updates for one message and returns
// the release. Per message rather than one lock for all of them: a Zulip call
// that hangs for zulipTimeout must not hold up another call's indicator, its
// recorder's join, or the shutdown waiting on it. r.mu guards only the map
// lookup and the refcount, never the wait — an entry lives exactly as long as
// somebody holds or queues on it.
func (r *Runner) lockIndicator(msgID int64) func() {
	r.mu.Lock()
	ind := r.indicators[msgID]
	if ind == nil {
		ind = &indicator{}
		r.indicators[msgID] = ind
	}
	ind.waiting++
	r.mu.Unlock()

	ind.mu.Lock()
	return func() {
		ind.mu.Unlock()
		r.mu.Lock()
		defer r.mu.Unlock()
		ind.waiting--
		if ind.waiting == 0 {
			delete(r.indicators, msgID)
		}
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
func (r *Runner) Resume() {
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
			// The same ending every other failure gets, except that the Zulip
			// half runs on its own goroutine: repairs must not hold the startup
			// path — and with it the signal handler — for a Zulip timeout each.
			err := r.saveFinalState(job)
			go r.announce(job, err)
		case job.State == JobFinished && job.WebhookSentAt == nil:
			slog.Info("retrying webhook after restart", "job", job.ID)
			if r.onFinished != nil {
				r.onFinished(job)
			}
		}
	}
}

// stampDelivered records the delivery in job.json — but re-reads first, because
// delivery can retry for minutes and a second 🎙️ click re-records into the same
// directory meanwhile. Writing the stale copy back would resurrect "finished"
// over a live recording. StartedAt is the generation marker: Start stamps it
// afresh every time, and the runner's mutex covers the re-read and the write
// together, so an admission cannot slip between them.
func (r *Runner) stampDelivered(job Job) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur, err := loadJob(r.cfg.DataDir, job.ID)
	if err != nil || cur.State != JobFinished || !cur.StartedAt.Equal(job.StartedAt) {
		slog.Info("webhook delivered for a superseded job", "job", job.ID)
		return
	}
	now := time.Now()
	cur.WebhookSentAt = &now
	if err := cur.save(r.cfg.DataDir); err != nil {
		slog.Error("saving job", "job", job.ID, "err", err)
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
