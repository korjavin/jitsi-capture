package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
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

	// ponytail: wiring lands in the integration bead.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("jitsi-capture stopped")
}

// logLevel parses LOG_LEVEL; anything unrecognised falls back to INFO.
func logLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
