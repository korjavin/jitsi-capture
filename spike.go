package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The Google Meet spike (bead jitsi2outline-8kp) run from Zulip: a DM
// "meet-spike <meet url> [seconds=N]" runs recorder/spike/meet-spike.js as a
// guest and posts its diagnostic lines plus the mixed audio back into the DM.
// ponytail: throwaway diagnostics; delete this file with the spike script.

const (
	spikeCommand        = "meet-spike"
	spikeDefaultSeconds = 90
	spikeMaxSeconds     = 600
	spikeJoinTimeoutS   = 300 // lobby wait, passed to the script as --join-timeout
	spikeSlackS         = 90  // browser launch + shutdown on top of join + record
	spikeLineMax        = 600 // per posted line; 15 lines + header stay under Zulip's 10k limit
)

var spikeRe = regexp.MustCompile(`^meet-spike\s+<?(https://meet\.google\.com/[A-Za-z0-9\-]+)>?(?:\s+seconds=(\d+))?$`)

// parseSpike reads "meet-spike <url> [seconds=N]". seconds defaults to 90 and is
// capped at 600. The URL is cut at the meeting code, so no query string reaches
// the script.
func parseSpike(content string) (url string, seconds int, ok bool) {
	m := spikeRe.FindStringSubmatch(strings.TrimSpace(content))
	if m == nil {
		return "", 0, false
	}
	seconds = spikeDefaultSeconds
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil || n < 1 {
			return "", 0, false
		}
		seconds = min(n, spikeMaxSeconds)
	}
	return m[1], seconds, true
}

type spikeZulip interface {
	Reply(ctx context.Context, job Job, content string) error
	Upload(ctx context.Context, name string, r io.Reader) (string, error)
}

// Spike runs one meet-spike.js child at a time.
type Spike struct {
	ctx     context.Context // cancelled on shutdown: SIGINTs the child
	bg      context.Context // outlives ctx, for the final reply
	z       spikeZulip
	script  string
	name    string
	dataDir string
	busy    atomic.Bool
	wg      sync.WaitGroup // every goroutine Handle starts
}

func newSpike(ctx, bg context.Context, cfg Config, z spikeZulip) *Spike {
	return &Spike{
		ctx: ctx, bg: bg, z: z, name: cfg.BotDisplayName, dataDir: cfg.DataDir,
		script: filepath.Join(filepath.Dir(cfg.RecorderPath), "spike", "meet-spike.js"),
	}
}

// Handle answers one "meet-spike ..." DM. It never blocks on the child.
func (s *Spike) Handle(m Message) {
	to := Job{DMUserID: m.SenderID}
	url, seconds, ok := parseSpike(m.Content)
	if !ok {
		s.async(func() {
			s.reply(to, "usage: `meet-spike https://meet.google.com/xxx-yyyy-zzz [seconds=N]` (default 90, max 600)")
		})
		return
	}
	if !s.busy.CompareAndSwap(false, true) {
		s.async(func() { s.reply(to, "busy: a meet-spike run is already in progress") })
		return
	}
	s.async(func() {
		defer s.busy.Store(false)
		s.reply(to, fmt.Sprintf("meet-spike: joining as guest for %d s, admit me from the lobby (I wait up to %d s).", seconds, spikeJoinTimeoutS))
		s.reply(to, s.run(url, seconds))
	})
}

func (s *Spike) async(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// Wait gives the goroutines Handle started — a run that shutdown interrupted
// included — up to d to stop the child, upload the audio and reply.
func (s *Spike) Wait(d time.Duration) {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		slog.Warn("meet-spike still running at shutdown")
	}
}

func (s *Spike) reply(to Job, text string) {
	ctx, cancel := context.WithTimeout(s.bg, zulipTimeout)
	defer cancel()
	if err := s.z.Reply(ctx, to, text); err != nil {
		slog.Error("meet-spike reply", "err", err)
	}
}

// run executes the script and returns the report to post.
func (s *Spike) run(url string, seconds int) string {
	outDir := filepath.Join(s.dataDir, "spike", time.Now().UTC().Format("20060102-150405"))
	// ponytail: spike dirs are not swept by retention; delete them by hand.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "meet-spike failed: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(seconds+spikeJoinTimeoutS+spikeSlackS)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodeBin, s.script, url,
		"--seconds", strconv.Itoa(seconds),
		"--join-timeout", strconv.Itoa(spikeJoinTimeoutS),
		"--captions",
		"--name", s.name,
		"--out-dir", outDir,
	)
	// SIGINT is the script's clean stop: it keeps the audio recorded so far.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 20 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	slog.Info("meet-spike started", "seconds", seconds, "out_dir", outDir)
	err := cmd.Run()
	code := exitCode(err)
	if ctx.Err() != nil {
		code = -1
	}
	slog.Info("meet-spike finished", "exit_code", code)

	var b strings.Builder
	fmt.Fprintf(&b, "meet-spike finished, exit code %d (0 recorded, 2 bad args, 3 never admitted, 4 error, -1 timeout/shutdown)\n```text\n%s\n```\n", code, spikeLines(stderr.String()))
	b.WriteString(s.uploadAudio(filepath.Join(outDir, "mixed.webm")))
	return b.String()
}

func (s *Spike) uploadAudio(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "No audio file."
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() == 0 {
		return "Audio file is empty."
	}
	ctx, cancel := context.WithTimeout(s.bg, 2*time.Minute)
	defer cancel()
	link, err := s.z.Upload(ctx, "meet-spike.webm", f)
	if err != nil {
		slog.Error("meet-spike audio upload", "err", err)
		return "Audio upload failed: " + err.Error()
	}
	return "Audio: [meet-spike.webm](" + link + ")"
}

// spikeLines keeps the lines of the script's log the go/no-go report needs:
// the last 8 STATE lines, the last 3 media and names samples, and SUMMARY (whose result field carries a fatal error).
// Anything else (an argument error, a crash) falls back to the log's tail.
func spikeLines(log string) string {
	var states, media, names, rest, all []string
	for _, l := range strings.Split(strings.TrimSpace(log), "\n") {
		if len(l) > spikeLineMax {
			l = strings.ToValidUTF8(l[:spikeLineMax], "") + "…"
		}
		all = append(all, l)
		switch {
		case strings.Contains(l, "] STATE "):
			states = append(states, l)
		case strings.Contains(l, "] media "):
			media = append(media, l)
		case strings.Contains(l, "] names "):
			names = append(names, l)
		case strings.Contains(l, "] SUMMARY"):
			rest = append(rest, l)
		}
	}
	tail := func(a []string, n int) []string { return a[max(0, len(a)-n):] }
	out := append(append(append(tail(states, 8), tail(media, 3)...), tail(names, 3)...), rest...)
	if len(out) == 0 {
		out = tail(all, 8)
	}
	return strings.ReplaceAll(strings.Join(out, "\n"), "```", "'''")
}
