package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	// shutdownGrace is what the HTTP server gets to finish in-flight requests.
	shutdownGrace = 5 * time.Second
	// recorderGrace is what the recorders get to finalize their files after
	// SIGTERM. compose stop_grace_period is 120 s, so this leaves room for the
	// webhook that a signalled — and therefore finished — recording produces.
	recorderGrace = 110 * time.Second
	// sweepEvery drives retention plus the webhook backstop.
	// ponytail: hourly is fine, the in-process backoff already covers minutes.
	sweepEvery = time.Hour
)

func main() {
	cfg, err := loadConfig()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)})))
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	webhook := "no"
	if cfg.WebhookURL != "" {
		webhook = "yes"
	}
	slog.Info("jitsi-capture starting",
		"data_dir", cfg.DataDir,
		"listen_addr", cfg.ListenAddr,
		"webhook", webhook)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, nil); err != nil {
		slog.Error("jitsi-capture failed", "err", err)
		os.Exit(1)
	}
	slog.Info("jitsi-capture stopped")
}

// run wires everything together and blocks on the Zulip event loop until ctx
// ends. ready, when set, receives the HTTP listener's address once it is up —
// the seam the wiring test needs to address a :0 port; main passes nil.
func run(ctx context.Context, cfg Config, ready func(net.Addr)) error {
	z := newZulip(cfg)

	// The webhook outlives ctx on purpose: SIGTERM makes the recorder finalize
	// and exit 0, so the job that shutdown produces is precisely the one worth
	// delivering. sendWebhook's backoff table bounds the attempt.
	hookCtx := context.WithoutCancel(ctx)
	onFinished := func(job Job) {
		if err := sendWebhook(hookCtx, cfg, job); err != nil {
			slog.Warn("webhook not delivered, will retry after the next sweep", "job", job.ID, "err", err)
			return
		}
		stampDelivered(cfg, job)
	}

	runner := newRunner(cfg, z, onFinished)
	sweepRetention(cfg.DataDir, cfg.AudioRetentionDays)

	srv := newHTTPServer(cfg.ListenAddr, (&server{
		secret:  cfg.WebhookSecret,
		loadJob: func(id string) (Job, error) { return loadJob(cfg.DataDir, id) },
		send:    z.SendMessage,
	}).handler())
	// Listen before serving so a busy port is a startup failure, not a log line.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	if ready != nil {
		ready(ln.Addr())
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server stopped", "err", err)
		}
	}()

	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	go func() {
		// Resume runs behind the listener, not in front of it: one unsent webhook
		// to a receiver that is down costs the whole backoff table, and a health
		// probe that hangs for minutes gets the container killed instead of
		// recovered. ponytail: a 🎙️ click landing in that window reads a stale
		// "recording" and is a silent no-op — the next click works. Split Resume
		// into repair + resend if that ever matters.
		runner.Resume(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				sweepRetention(cfg.DataDir, cfg.AudioRetentionDays)
				resendUnsent(cfg, onFinished)
			}
		}
	}()

	runErr := newBot(cfg, z, runner.Start).Run(ctx)

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Warn("http shutdown", "err", err)
	}
	runner.Stop(recorderGrace)
	return runErr
}

// stampDelivered records the delivery in job.json — but re-reads first, because
// delivery can retry for minutes and a second 🎙️ click re-records into the same
// directory meanwhile. Writing the stale copy back would resurrect "finished"
// over a live recording. StartedAt is the generation marker: Start stamps it
// afresh every time.
// ponytail: a Start landing between the read and the save still wins the
// clobber; put the stamp behind the runner's mutex if that ever bites.
func stampDelivered(cfg Config, job Job) {
	cur, err := loadJob(cfg.DataDir, job.ID)
	if err != nil || cur.State != JobFinished || !cur.StartedAt.Equal(job.StartedAt) {
		slog.Info("webhook delivered for a superseded job", "job", job.ID)
		return
	}
	now := time.Now()
	cur.WebhookSentAt = &now
	if err := cur.save(cfg.DataDir); err != nil {
		slog.Error("saving job", "job", job.ID, "err", err)
	}
}

// resendUnsent hands every finished job whose webhook never went out back to the
// sender. ponytail: an in-flight retry can overlap this one, so the receiver is
// expected to be idempotent on the job id — it already has to be, the backoff
// can deliver twice on a timeout.
func resendUnsent(cfg Config, onFinished func(Job)) {
	jobs, err := listJobs(cfg.DataDir)
	if err != nil {
		slog.Error("listing jobs for the webhook sweep", "err", err)
		return
	}
	for _, job := range jobs {
		if job.State == JobFinished && job.WebhookSentAt == nil {
			slog.Info("retrying webhook", "job", job.ID)
			onFinished(job)
		}
	}
}

// logLevel parses LOG_LEVEL; anything unrecognised falls back to INFO.
func logLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
