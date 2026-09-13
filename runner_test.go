package main

import (
	"context"
	"errors"
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

type zulipCall struct {
	msgID         int64
	emoji         string
	stream, topic string
	content       string
	isRemoveReact bool
}

type fakeZulip struct {
	mu           sync.Mutex
	calls        []zulipCall
	err          error
	delay        time.Duration // stands in for a slow Zulip during shutdown
	beforeRemove func()        // runs at the start of RemoveReaction
}

func (f *fakeZulip) RemoveReaction(_ context.Context, msgID int64, emoji string) error {
	if f.beforeRemove != nil {
		f.beforeRemove()
	}
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zulipCall{msgID: msgID, emoji: emoji, isRemoveReact: true})
	return f.err
}

func (f *fakeZulip) SendMessage(_ context.Context, stream, topic, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, zulipCall{stream: stream, topic: topic, content: content})
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
		if !c.isRemoveReact {
			out = append(out, c.content)
		}
	}
	return out
}

func (f *fakeZulip) reactionRemoved(msgID int64) bool {
	for _, c := range f.snapshot() {
		if c.isRemoveReact && c.msgID == msgID && c.emoji == recordingEmoji {
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
	return newRunner(cfg, z, func(j Job) { finished <- j }), z, finished
}

// waitSettled polls job.json until the job leaves the recording state. The
// failure note is posted before that; the reaction removal follows the save, so
// assert it with waitReactionRemoved.
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

// waitReactionRemoved polls for the recording reaction being dropped.
func waitReactionRemoved(t *testing.T, z *fakeZulip, msgID int64) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if z.reactionRemoved(msgID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("recording reaction on %d was not removed", msgID)
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
	if calls := z.snapshot(); len(calls) != 0 {
		t.Errorf("duplicate Start talked to Zulip: %+v", calls)
	}
	select {
	case j := <-finished:
		t.Errorf("duplicate Start finished a job: %+v", j)
	default:
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
	if !z.reactionRemoved(job.MessageID) {
		t.Error("recording reaction was not removed")
	}
	if msgs := z.messages(); len(msgs) != 0 {
		t.Errorf("a success posted a note: %v", msgs)
	}
	if saved := waitSettled(t, r.cfg.DataDir, job.ID); saved.State != JobFinished || saved.DurationS != 120 {
		t.Errorf("job.json = %+v, want finished/120", saved)
	}

	select {
	case again := <-finished:
		t.Errorf("onFinished called twice: %+v", again)
	default:
	}
}

// The on-disk state must be final before the recording reaction goes away:
// otherwise a 🎙️ click arriving in that window reads "recording", Start rejects
// it as a duplicate, and the bot's 🔴 re-add outlives the removal.
func TestSettleSavesBeforeClearingReaction(t *testing.T) {
	r, z, finished := newTestRunner(t, "rec_ok.sh")
	job := testJob()
	var stateAtRemoval string
	z.beforeRemove = func() {
		if j, err := loadJob(r.cfg.DataDir, job.ID); err == nil {
			stateAtRemoval = j.State
		}
	}
	if err := r.Start(job); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("onFinished was never called")
	}
	if stateAtRemoval != JobFinished {
		t.Errorf("job.json said %q while the reaction was removed, want %q", stateAtRemoval, JobFinished)
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

	r.Resume(context.Background())

	got, err := loadJob(dir, interrupted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobFailed || got.Error != errInterrupted || got.EndedAt == nil {
		t.Errorf("interrupted job = %+v, want failed/interrupted with ended_at", got)
	}
	if msgs := z.messages(); len(msgs) != 1 || msgs[0] != failNote[errInterrupted] {
		t.Errorf("notes = %v, want the interrupted line", msgs)
	}
	if !z.reactionRemoved(interrupted.MessageID) {
		t.Error("recording reaction was not removed for the interrupted job")
	}

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
