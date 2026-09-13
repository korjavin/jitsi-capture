package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// The fake recorders in testdata/ are shell scripts, so the tests run them with
// sh instead of node. Set once, in init, so no test goroutine ever races a
// later assignment.
func init() { nodeBin = "sh" }

// zulipCall kinds.
const (
	callAdd     = "add"
	callRemove  = "remove"
	callMessage = "message"
)

type zulipCall struct {
	kind          string
	msgID         int64
	emoji         string
	stream, topic string
	content       string
}

type fakeZulip struct {
	mu           sync.Mutex
	calls        []zulipCall
	err          error
	delay        time.Duration // stands in for a slow Zulip during shutdown
	beforeRemove func(int64)   // runs at the start of RemoveReaction, with the message id
	beforeSend   func()        // runs at the start of SendMessage
}

func (f *fakeZulip) AddReaction(_ context.Context, msgID int64, emoji string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zulipCall{kind: callAdd, msgID: msgID, emoji: emoji})
	return f.err
}

func (f *fakeZulip) RemoveReaction(_ context.Context, msgID int64, emoji string) error {
	if f.beforeRemove != nil {
		f.beforeRemove(msgID)
	}
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zulipCall{kind: callRemove, msgID: msgID, emoji: emoji})
	return f.err
}

func (f *fakeZulip) SendMessage(_ context.Context, stream, topic, content string) error {
	if f.beforeSend != nil {
		f.beforeSend()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zulipCall{kind: callMessage, stream: stream, topic: topic, content: content})
	return f.err
}

func (f *fakeZulip) snapshot() []zulipCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]zulipCall(nil), f.calls...)
}

func (f *fakeZulip) messages() []string {
	var out []string
	for _, c := range f.snapshot() {
		if c.kind == callMessage {
			out = append(out, c.content)
		}
	}
	return out
}

// indicatorLog is every add/remove of the recording indicator on msgID, in
// order — the sequence a viewer of the message would have seen.
func (f *fakeZulip) indicatorLog(msgID int64) []string {
	var out []string
	for _, c := range f.snapshot() {
		if c.kind != callMessage && c.msgID == msgID && c.emoji == recordingEmoji {
			out = append(out, c.kind)
		}
	}
	return out
}

func (f *fakeZulip) reactionRemoved(msgID int64) bool {
	for _, c := range f.snapshot() {
		if c.kind == callRemove && c.msgID == msgID && c.emoji == recordingEmoji {
			return true
		}
	}
	return false
}

func testJob() Job {
	return Job{ID: "42", MessageID: 42, Stream: "general", Topic: "standup", JitsiURL: "https://example.invalid/Room42"}
}

// newTestRunner wires a runner against one of the fake recorders. finished
// receives every job handed to onFinished.
func newTestRunner(t *testing.T, script string) (*Runner, *fakeZulip, chan Job) {
	t.Helper()
	cfg := Config{
		DataDir:        t.TempDir(),
		RecorderPath:   filepath.Join("testdata", script),
		BotDisplayName: "NoteTaker",
		JoinTimeoutS:   600,
		MaxDurationS:   14400,
		EmptyGraceS:    60,
		MinRecordingS:  15,
	}
	z := &fakeZulip{}
	finished := make(chan Job, 4)
	return newRunner(context.Background(), cfg, z, func(j Job) { finished <- j }), z, finished
}

// waitSettled polls job.json until the job leaves the recording state. The note
// and the indicator both follow that save, so assert them with
// waitReactionRemoved and the fake's hooks.
func waitSettled(t *testing.T, dataDir, id string) Job {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if j, err := loadJob(dataDir, id); err == nil && j.State != JobRecording {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never left the recording state", id)
	return Job{}
}

// eventually polls cond until it holds, for the work the runner hands to a
// goroutine so that the event loop and the startup path never wait on Zulip.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("timed out waiting for %s", what)
}

// waitReactionRemoved polls for the recording reaction being dropped.
func waitReactionRemoved(t *testing.T, z *fakeZulip, msgID int64) {
	t.Helper()
	eventually(t, fmt.Sprintf("the recording reaction on %d to be removed", msgID), func() bool {
		return z.reactionRemoved(msgID)
	})
}

func TestJobSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ended := time.Now().Add(-time.Minute).UTC().Round(time.Second)
	want := testJob()
	want.State = JobFinished
	want.StartedAt = ended.Add(-2 * time.Minute)
	want.EndedAt = &ended
	want.DurationS = 120
	want.AudioPath = filepath.Join(jobDir(dir, want.ID), "audio.webm")
	want.Participants = []string{"Alice", "Bob"}
	want.Tracks = []Track{{ID: "t1", Name: "Alice", Path: "a.webm", OffsetS: 1.5, EndedS: 9}}

	if err := want.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadJob(dir, want.ID)
	if err != nil {
		t.Fatalf("loadJob: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch\n got %+v\nwant %+v", got, want)
	}

	// Atomic: the temp file is gone, only job.json remains.
	ents, err := os.ReadDir(jobDir(dir, want.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "job.json" {
		t.Errorf("save left partial files behind: %v", ents)
	}
}

func TestLoadJobMissing(t *testing.T) {
	if _, err := loadJob(t.TempDir(), "nope"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("want os.ErrNotExist, got %v", err)
	}
}

func TestListJobsSkipsUnreadable(t *testing.T) {
	dir := t.TempDir()
	if jobs, err := listJobs(dir); err != nil || jobs != nil {
		t.Fatalf("empty data dir: got %v, %v", jobs, err)
	}
	j := testJob()
	if err := j.save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jobDir(dir, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir(dir, "broken"), "job.json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs, err := listJobs(dir)
	if err != nil {
		t.Fatalf("listJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != j.ID {
		t.Errorf("want only job %s, got %+v", j.ID, jobs)
	}
}

func TestStartRejectsDuplicate(t *testing.T) {
	r, z, finished := newTestRunner(t, "rec_ok.sh")
	inflight := testJob()
	inflight.State = JobRecording
	if err := inflight.save(r.cfg.DataDir); err != nil {
		t.Fatal(err)
	}

	if err := r.Start(testJob()); !errors.Is(err, ErrDuplicateJob) {
		t.Fatalf("want ErrDuplicateJob, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobDir(r.cfg.DataDir, inflight.ID), "audio.webm")); !errors.Is(err, os.ErrNotExist) {
		t.Error("duplicate Start spawned a recorder")
	}
	// It re-asserts the indicator instead — off the event loop, so the click
	// costs the bot nothing: the job is still recording, so an add that failed
	// transiently the first time is repaired by the second click.
	eventually(t, "the indicator re-assert", func() bool {
		return reflect.DeepEqual(z.indicatorLog(inflight.MessageID), []string{callAdd})
	})
	if msgs := z.messages(); len(msgs) != 0 {
		t.Errorf("duplicate Start posted a note: %v", msgs)
	}
	select {
	case j := <-finished:
		t.Errorf("duplicate Start finished a job: %+v", j)
	default:
	}
}

// A Start that never took hold must leave nothing behind — no 🔴 on a message
// whose recording does not exist.
func TestStartThatCannotSaveTouchesNoReactions(t *testing.T) {
	r, z, _ := newTestRunner(t, "rec_ok.sh")
	job := testJob()
	// A regular file where the job directory belongs: save cannot create it.
	if err := os.MkdirAll(filepath.Join(r.cfg.DataDir, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobDir(r.cfg.DataDir, job.ID), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := r.Start(job)
	if err == nil || errors.Is(err, ErrDuplicateJob) {
		t.Fatalf("Start = %v, want a save failure", err)
	}
	if calls := z.snapshot(); len(calls) != 0 {
		t.Errorf("a Start that did not take hold talked to Zulip: %+v", calls)
	}
}

func TestStartRerecordsFailedJob(t *testing.T) {
	r, _, finished := newTestRunner(t, "rec_ok.sh")
	old := testJob()
	old.State = JobFailed
	old.Error = errNotAdmitted
	if err := old.save(r.cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(testJob()); err != nil {
		t.Fatalf("Start after a failed run: %v", err)
	}
	if got := (<-finished).State; got != JobFinished {
		t.Errorf("state = %q, want finished", got)
	}
}

func TestRunSuccess(t *testing.T) {
	r, z, finished := newTestRunner(t, "rec_ok.sh")
	job := testJob()
	if err := r.Start(job); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got Job
	select {
	case got = <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("onFinished was never called")
	}
	if got.State != JobFinished || got.Error != "" {
		t.Errorf("state = %q error = %q, want finished", got.State, got.Error)
	}
	if got.DurationS != 120 {
		t.Errorf("duration = %v, want 120", got.DurationS)
	}
	if want := []string{"Alice", "Bob"}; !reflect.DeepEqual(got.Participants, want) {
		t.Errorf("participants = %v, want %v", got.Participants, want)
	}
	if got.EndedAt == nil {
		t.Error("ended_at not set")
	}
	if b, err := os.ReadFile(got.AudioPath); err != nil || len(b) == 0 {
		t.Errorf("audio file: %v (%d bytes)", err, len(b))
	}
	// The fake recorder echoes back the --tracks-dir it was given, so this also
	// pins where the runner tells the recorder to put per-participant audio.
	wantTracks := []Track{{
		ID:      "p1",
		Name:    "Alice",
		Path:    filepath.Join(jobDir(r.cfg.DataDir, job.ID), "tracks", "p1.webm"),
		OffsetS: 1.5,
		EndedS:  119,
	}}
	if !reflect.DeepEqual(got.Tracks, wantTracks) {
		t.Errorf("tracks = %+v, want %+v", got.Tracks, wantTracks)
	}
	if b, err := os.ReadFile(wantTracks[0].Path); err != nil || len(b) == 0 {
		t.Errorf("track file: %v (%d bytes)", err, len(b))
	}
	if !z.reactionRemoved(job.MessageID) {
		t.Error("recording reaction was not removed")
	}
	if msgs := z.messages(); len(msgs) != 0 {
		t.Errorf("a success posted a note: %v", msgs)
	}
	saved := waitSettled(t, r.cfg.DataDir, job.ID)
	if saved.State != JobFinished || saved.DurationS != 120 {
		t.Errorf("job.json = %+v, want finished/120", saved)
	}
	if !reflect.DeepEqual(saved.Tracks, wantTracks) {
		t.Errorf("job.json tracks = %+v, want %+v", saved.Tracks, wantTracks)
	}

	select {
	case again := <-finished:
		t.Errorf("onFinished called twice: %+v", again)
	default:
	}
}

// Nothing reaches Zulip before the final state is on disk. A reader of the
// failure note who clicks again must find a job that can be re-recorded rather
// than a silent duplicate, and a click racing the indicator must see the same.
func TestSettleSavesBeforeTalkingToZulip(t *testing.T) {
	for _, tc := range []struct {
		name, script, wantState string
	}{
		{"a finished recording", "rec_ok.sh", JobFinished},
		{"a failed one, which also posts a note", "rec_exit3.sh", JobFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, z, _ := newTestRunner(t, tc.script)
			job := testJob()
			stateOnDisk := func() string {
				j, err := loadJob(r.cfg.DataDir, job.ID)
				if err != nil {
					return err.Error()
				}
				return j.State
			}
			var atNote, atIndicator string
			z.beforeSend = func() { atNote = stateOnDisk() }
			z.beforeRemove = func(int64) { atIndicator = stateOnDisk() }

			if err := r.Start(job); err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitSettled(t, r.cfg.DataDir, job.ID)
			waitReactionRemoved(t, z, job.MessageID)

			if atIndicator != tc.wantState {
				t.Errorf("job.json said %q while the indicator was cleared, want %q", atIndicator, tc.wantState)
			}
			if tc.wantState == JobFailed && atNote != tc.wantState {
				t.Errorf("job.json said %q while the note was posted, want %q", atNote, tc.wantState)
			}
		})
	}
}

// A 🎙️ click landing while a finished job's 🔴 is coming off must not lose the
// new recording's own 🔴 to that removal. The indicator calls are serialized, so
// the new one's add lands after the removal rather than under it: the message a
// viewer sees alternates add/remove, never add/add.
func TestStartAndSettleSerializeTheIndicator(t *testing.T) {
	r, z, finished := newTestRunner(t, "rec_ok.sh")
	job := testJob()

	var once sync.Once
	reStart := make(chan error, 1)
	z.beforeRemove = func(int64) {
		once.Do(func() { // only the first settlement is raced
			go func() { reStart <- r.Start(testJob()) }()
			// Long enough for an unserialized Start to save, add its 🔴, and
			// then lose it to the removal that follows.
			time.Sleep(100 * time.Millisecond)
		})
	}

	if err := r.Start(job); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 1; i <= 2; i++ {
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Fatalf("recording %d never finished", i)
		}
	}
	if err := <-reStart; err != nil {
		t.Fatalf("re-record Start: %v", err)
	}

	want := []string{callAdd, callRemove, callAdd, callRemove}
	if got := z.indicatorLog(job.MessageID); !reflect.DeepEqual(got, want) {
		t.Errorf("indicator changes = %v, want %v", got, want)
	}
}

// The indicator lock is per message, not one for all of them: a Zulip call that
// hangs on one message must not hold up another message's indicator — which is
// how a handful of jobs finalizing at once would push a shutdown past its grace.
func TestIndicatorLockIsPerMessage(t *testing.T) {
	r, z, _ := newTestRunner(t, "rec_ok.sh")
	stuck, other := testJob(), testJob()
	other.ID, other.MessageID = "43", 43
	for _, j := range []Job{stuck, other} {
		j.State = JobFinished
		if err := j.save(r.cfg.DataDir); err != nil {
			t.Fatal(err)
		}
	}

	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})
	z.beforeRemove = func(msgID int64) {
		if msgID != stuck.MessageID {
			return
		}
		close(entered)
		<-release
	}

	go r.syncIndicator(stuck, slog.LevelError)
	<-entered // that message's removal is in flight and going nowhere

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.syncIndicator(other, slog.LevelError)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung indicator call on one message blocked another message's")
	}
}

// Reading the state back is right only while the state is actually there. When
// the settlement could not save, job.json still says "recording", so the
// indicator has to come off on the strength of the in-memory job instead — or
// it would be put back on a recording that has ended, for good.
func TestSettleWithAFailedSaveStillClearsTheIndicator(t *testing.T) {
	r, z, _ := newTestRunner(t, "rec_ok.sh")
	live := testJob()
	live.State = JobRecording
	if err := live.save(r.cfg.DataDir); err != nil {
		t.Fatal(err)
	}

	ended := live
	ended.State = JobFailed
	ended.Error = errRecorderFailed
	r.announce(ended, errors.New("no space left on device"))

	if got, want := z.indicatorLog(live.MessageID), []string{callRemove}; !reflect.DeepEqual(got, want) {
		t.Errorf("indicator changes = %v, want %v: the stale job.json won", got, want)
	}
}

// Serializing the calls is only half of it: each one reads the state on disk
// rather than trusting the copy its caller carries. That is what lets a
// settlement whose removal runs after a re-click put the indicator back instead
// of stripping it off a recording that has just started.
func TestSyncIndicatorFollowsTheStateOnDisk(t *testing.T) {
	r, z, _ := newTestRunner(t, "rec_ok.sh")
	job := testJob()
	job.State = JobRecording
	if err := job.save(r.cfg.DataDir); err != nil {
		t.Fatal(err)
	}

	stale := job
	stale.State = JobFinished // what a settlement that lost the race carries
	r.syncIndicator(stale, slog.LevelError)

	if got, want := z.indicatorLog(job.MessageID), []string{callAdd}; !reflect.DeepEqual(got, want) {
		t.Errorf("indicator changes = %v, want %v: a live recording lost its indicator", got, want)
	}
}

// stampDelivered shares the runner's mutex with Start, so a delivery that read
// job.json before a re-record cannot write it back afterwards.
func TestStampDeliveredWaitsForTheRunnerMutex(t *testing.T) {
	r, _, _ := newTestRunner(t, "rec_ok.sh")
	delivered := testJob()
	delivered.State = JobFinished
	delivered.StartedAt = time.Now().Add(-time.Hour)
	if err := delivered.save(r.cfg.DataDir); err != nil {
		t.Fatal(err)
	}

	r.mu.Lock() // stands in for the Start that is admitting the re-record
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.stampDelivered(delivered)
	}()
	select {
	case <-done:
		r.mu.Unlock()
		t.Fatal("stampDelivered ran without the runner mutex: a Start can still land between its read and its save")
	case <-time.After(100 * time.Millisecond):
	}

	// What that Start would have written: the same directory, a new generation.
	next := testJob()
	next.State = JobRecording
	next.StartedAt = time.Now()
	if err := next.save(r.cfg.DataDir); err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	r.mu.Unlock()
	<-done

	got, err := loadJob(r.cfg.DataDir, delivered.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobRecording || got.WebhookSentAt != nil {
		t.Errorf("job.json = %+v, want the re-record untouched", got)
	}
}

// A delivery that lands after the message was re-recorded must not write its
// stale copy back over the new recording.
func TestStampDeliveredSkipsSupersededJob(t *testing.T) {
	cfg := Config{DataDir: t.TempDir()}
	old := testJob()
	old.State = JobFinished
	old.StartedAt = time.Now().Add(-time.Hour)

	current := old
	current.State = JobRecording
	current.StartedAt = time.Now()
	if err := current.save(cfg.DataDir); err != nil {
		t.Fatal(err)
	}

	r := newRunner(context.Background(), cfg, nil, nil)
	r.stampDelivered(old)

	got, err := loadJob(cfg.DataDir, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobRecording || got.WebhookSentAt != nil {
		t.Errorf("job.json = %+v, want the re-record untouched", got)
	}

	// The same job, not superseded, is stamped.
	current.State = JobFinished
	if err := current.save(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	r.stampDelivered(current)
	if got, err = loadJob(cfg.DataDir, current.ID); err != nil || got.WebhookSentAt == nil {
		t.Errorf("job.json = %+v (%v), want webhook_sent_at", got, err)
	}
}

func TestRunFailures(t *testing.T) {
	for _, tc := range []struct {
		name, script, wantErr string
	}{
		{"not admitted", "rec_exit3.sh", errNotAdmitted},
		{"too short", "rec_short.sh", errTooShort},
		{"missing recorder", "rec_does_not_exist.sh", errRecorderFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, z, finished := newTestRunner(t, tc.script)
			job := testJob()
			if err := r.Start(job); err != nil {
				t.Fatalf("Start: %v", err)
			}
			got := waitSettled(t, r.cfg.DataDir, job.ID)
			if got.State != JobFailed || got.Error != tc.wantErr {
				t.Errorf("state = %q error = %q, want failed/%s", got.State, got.Error, tc.wantErr)
			}
			if msgs := z.messages(); len(msgs) != 1 || msgs[0] != failNote[tc.wantErr] {
				t.Errorf("notes = %v, want %q", msgs, failNote[tc.wantErr])
			}
			waitReactionRemoved(t, z, job.MessageID)
			select {
			case j := <-finished:
				t.Errorf("onFinished called for a failed job: %+v", j)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// Stop must cover a job accepted a moment earlier and must not return until
// that job's final state is on disk — otherwise the next Resume would mark a
// finished recording as interrupted.
func TestStopWaitsForSettlement(t *testing.T) {
	r, z, _ := newTestRunner(t, "rec_ok.sh")
	z.delay = 200 * time.Millisecond

	job := testJob()
	if err := r.Start(job); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.Stop(15 * time.Second)

	got, err := loadJob(r.cfg.DataDir, job.ID)
	if err != nil {
		t.Fatalf("loadJob after Stop: %v", err)
	}
	if got.State == JobRecording {
		t.Error("Stop returned while the job was still marked recording")
	}
	if got.EndedAt == nil {
		t.Error("Stop returned before ended_at was persisted")
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	r, _, _ := newTestRunner(t, "rec_ok.sh")
	r.Stop(time.Second) // must not block
}

func TestResume(t *testing.T) {
	r, z, finished := newTestRunner(t, "rec_ok.sh")
	dir := r.cfg.DataDir
	sent := time.Now().Add(-time.Hour)
	ended := time.Now().Add(-2 * time.Hour)

	interrupted := testJob()
	interrupted.State = JobRecording

	pending := testJob()
	pending.ID, pending.MessageID = "43", 43
	pending.State = JobFinished
	pending.EndedAt = &ended

	done := testJob()
	done.ID, done.MessageID = "44", 44
	done.State = JobFinished
	done.EndedAt = &ended
	done.WebhookSentAt = &sent

	for _, j := range []Job{interrupted, pending, done} {
		if err := j.save(dir); err != nil {
			t.Fatal(err)
		}
	}

	r.Resume()

	got, err := loadJob(dir, interrupted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobFailed || got.Error != errInterrupted || got.EndedAt == nil {
		t.Errorf("interrupted job = %+v, want failed/interrupted with ended_at", got)
	}
	// The job is repaired on disk before Resume returns; the Zulip half of it
	// follows on its own goroutine, so that the startup path stays interruptible.
	eventually(t, "the interrupted note", func() bool {
		msgs := z.messages()
		return len(msgs) == 1 && msgs[0] == failNote[errInterrupted]
	})
	waitReactionRemoved(t, z, interrupted.MessageID)

	select {
	case j := <-finished:
		if j.ID != pending.ID {
			t.Errorf("webhook retried for %s, want %s", j.ID, pending.ID)
		}
	default:
		t.Error("onFinished was not called for the unsent webhook")
	}
	select {
	case j := <-finished:
		t.Errorf("onFinished called again for %s", j.ID)
	default:
	}
}

func TestSweepRetention(t *testing.T) {
	dir := t.TempDir()
	oldTime := time.Now().AddDate(0, 0, -10)
	recent := time.Now().Add(-time.Hour)

	expired := testJob()
	expired.State = JobFinished
	expired.EndedAt = &oldTime

	fresh := testJob()
	fresh.ID, fresh.MessageID = "43", 43
	fresh.State = JobFinished
	fresh.EndedAt = &recent

	recording := testJob()
	recording.ID, recording.MessageID = "44", 44
	recording.State = JobRecording
	recording.StartedAt = oldTime

	for _, j := range []Job{expired, fresh, recording} {
		if err := j.save(dir); err != nil {
			t.Fatal(err)
		}
	}
	// A directory with no readable job.json falls back to its mtime.
	stray := jobDir(dir, "stray")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stray, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	sweepRetention(dir, 7)

	for _, id := range []string{fresh.ID, recording.ID} {
		if _, err := os.Stat(jobDir(dir, id)); err != nil {
			t.Errorf("job %s was removed: %v", id, err)
		}
	}
	for _, id := range []string{expired.ID, "stray"} {
		if _, err := os.Stat(jobDir(dir, id)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("job %s was kept: %v", id, err)
		}
	}
}

func TestParseResult(t *testing.T) {
	for _, tc := range []struct {
		name, stdout string
		wantErr      bool
		want         recResult
	}{
		{
			name:   "last non-empty line wins",
			stdout: "starting\n{\"out\":\"/d/a.webm\",\"duration_s\":12.5,\"reason\":\"signal\",\"participants\":[\"Alice\"]}\n\n",
			want:   recResult{Out: "/d/a.webm", DurationS: 12.5, Reason: "signal", Participants: []string{"Alice"}},
		},
		{
			name:   "tracks absent is fine",
			stdout: "{\"out\":\"/d/a.webm\",\"duration_s\":1}\n",
			want:   recResult{Out: "/d/a.webm", DurationS: 1},
		},
		{
			name:   "tracks present",
			stdout: "{\"duration_s\":1,\"tracks\":[{\"id\":\"t1\",\"name\":\"Alice\",\"path\":\"a.webm\",\"offset_s\":2,\"ended_s\":3}]}",
			want:   recResult{DurationS: 1, Tracks: []Track{{ID: "t1", Name: "Alice", Path: "a.webm", OffsetS: 2, EndedS: 3}}},
		},
		{name: "no output", stdout: "\n  \n", wantErr: true},
		{name: "not json", stdout: "boom\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseResult([]byte(tc.stdout))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
