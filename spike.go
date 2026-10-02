package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	spikeJoinTimeoutS   = 300      // lobby wait, passed to the script as --join-timeout
	spikeSlackS         = 90       // browser launch + shutdown on top of join + record
	spikeLineMax        = 600      // per posted line; 15 lines + header stay under Zulip's 10k limit
	spikeCaptionLines   = 20       // caption lines quoted in the DM; the rest is in captions.jsonl
	spikeCaptionMax     = 300      // per quoted caption line
	spikeUploadMax      = 24 << 20 // under Zulip's default 25 MB upload limit
)

var (
	spikeRe     = regexp.MustCompile(`^meet-spike\s+<?(https://meet\.google\.com/[A-Za-z0-9\-]+)>?((?:\s+\S+)*)$`)
	spikeLangRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`) // the script's --lang check
)

// parseSpike reads "meet-spike <url> [seconds=N] [lang=<code>]", options in any
// order. seconds defaults to 90 and is capped at 600; lang (the Meet caption
// language, e.g. en-US) defaults to Meet's own. The URL is cut at the meeting
// code, so no query string reaches the script.
func parseSpike(content string) (url string, seconds int, lang string, ok bool) {
	m := spikeRe.FindStringSubmatch(strings.TrimSpace(content))
	if m == nil {
		return "", 0, "", false
	}
	seconds = spikeDefaultSeconds
	for _, opt := range strings.Fields(m[2]) {
		k, v, _ := strings.Cut(opt, "=")
		switch k {
		case "seconds":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return "", 0, "", false
			}
			seconds = min(n, spikeMaxSeconds)
		case "lang":
			if !spikeLangRe.MatchString(v) {
				return "", 0, "", false
			}
			lang = v
		default:
			return "", 0, "", false
		}
	}
	return m[1], seconds, lang, true
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
	url, seconds, lang, ok := parseSpike(m.Content)
	if !ok {
		s.async(func() {
			s.reply(to, "usage: `meet-spike https://meet.google.com/xxx-yyyy-zzz [seconds=N] [lang=en-US]` (seconds: default 90, max 600; lang: Meet caption language, default Meet's)")
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
		report, captions := s.run(url, seconds, lang)
		s.reply(to, report)
		if captions != "" {
			s.reply(to, captions)
		}
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

// run executes the script and returns the report to post plus, when the run
// got that far, a second message with the captions (separate: the report alone
// is already close to Zulip's 10k-character limit).
func (s *Spike) run(url string, seconds int, lang string) (report, captions string) {
	outDir := filepath.Join(s.dataDir, "spike", time.Now().UTC().Format("20060102-150405"))
	// ponytail: spike dirs are not swept by retention; delete them by hand.
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "meet-spike failed: " + err.Error(), ""
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Duration(seconds+spikeJoinTimeoutS+spikeSlackS)*time.Second)
	defer cancel()
	args := []string{s.script, url,
		"--seconds", strconv.Itoa(seconds),
		"--join-timeout", strconv.Itoa(spikeJoinTimeoutS),
		"--captions",
		"--name", s.name,
		"--out-dir", outDir,
	}
	if lang != "" {
		args = append(args, "--lang", lang)
	}
	cmd := exec.CommandContext(ctx, nodeBin, args...)
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
	b.WriteString(s.upload("Audio", filepath.Join(outDir, "mixed.webm"), "meet-spike.webm") + "\n")
	b.WriteString(s.upload("Monitor audio", filepath.Join(outDir, "monitor.wav"), "meet-spike-monitor.wav") + "\n")
	b.WriteString(s.upload("Tab audio", filepath.Join(outDir, "tab.webm"), "meet-spike-tab.webm") + "\n")
	b.WriteString(s.upload("Log", filepath.Join(outDir, "spike.log"), "meet-spike.log"))
	return b.String(), s.captionsReport(filepath.Join(outDir, "captions.jsonl"))
}

// upload posts one file of the run to Zulip and returns the line for the DM.
func (s *Spike) upload(what, path, name string) string {
	f, err := os.Open(path)
	if err != nil {
		return "No " + strings.ToLower(what) + " file."
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return what + " file is empty."
	}
	if fi.Size() > spikeUploadMax {
		return fmt.Sprintf("%s file is %d MB, too big to upload; it stays in %s.", what, fi.Size()>>20, path)
	}
	ctx, cancel := context.WithTimeout(s.bg, 2*time.Minute)
	defer cancel()
	link, err := s.z.Upload(ctx, name, f)
	if err != nil {
		slog.Error("meet-spike upload", "file", name, "err", err)
		return what + " upload failed: " + err.Error()
	}
	return what + ": [" + name + "](" + link + ")"
}

// captionsReport renders the first spikeCaptionLines lines of captions.jsonl as
// "speaker: text" plus a link to the whole file.
func (s *Spike) captionsReport(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "Captions: no captions file."
	}
	var lines []string
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var c struct{ Speaker, Text string }
		if json.Unmarshal([]byte(l), &c) != nil || c.Text == "" {
			continue
		}
		n++
		if len(lines) < spikeCaptionLines {
			line := c.Speaker + ": " + c.Text
			if len(line) > spikeCaptionMax {
				line = strings.ToValidUTF8(line[:spikeCaptionMax], "") + "…"
			}
			lines = append(lines, line)
		}
	}
	if n == 0 {
		return "Captions: none captured."
	}
	return fmt.Sprintf("Captions: %d lines, the first %d:\n```text\n%s\n```\n%s", n, len(lines),
		strings.ReplaceAll(strings.Join(lines, "\n"), "```", "'''"),
		s.upload("Captions", path, "captions.jsonl"))
}

// spikeLines keeps the lines of the script's log the go/no-go report needs:
// the last 6 STATE lines, the last media, names, rtp and captions-text samples,
// the devices lines (prejoin and in-call, last 2), pulse, AUDIO (the level per
// capture method) and SUMMARY (whose result field carries a fatal error) — at
// most 15 lines. The whole log is uploaded as well.
// Anything else (an argument error, a crash) falls back to the log's tail.
func spikeLines(log string) string {
	var states, media, names, devices, rtp, capText, rest, all []string
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
		case strings.Contains(l, "] devices "):
			devices = append(devices, l)
		case strings.Contains(l, "] rtp {"):
			rtp = append(rtp, l)
		case strings.Contains(l, "] captions text "):
			capText = append(capText, l)
		case strings.Contains(l, "] SUMMARY"), strings.Contains(l, "] AUDIO"), strings.Contains(l, "] pulse: "):
			rest = append(rest, l)
		}
	}
	tail := func(a []string, n int) []string { return a[max(0, len(a)-n):] }
	var out []string
	for _, part := range [][]string{tail(states, 6), tail(devices, 2), tail(media, 1), tail(rtp, 1), tail(names, 1), tail(capText, 1), rest} {
		out = append(out, part...)
	}
	if len(out) == 0 {
		out = tail(all, 8)
	}
	return strings.ReplaceAll(strings.Join(out, "\n"), "```", "'''")
}
