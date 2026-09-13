package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// retryDelay is the pause after a failed register or events call.
const retryDelay = 5 * time.Second

// Bot is the Zulip event loop implementing the emoji-reaction flow: a stream
// message carrying a Jitsi URL gets a microphone reaction, and somebody clicking
// that reaction starts a recording.
type Bot struct {
	z       *Zulip
	botID   int64
	jitsiRe *regexp.Regexp
	start   func(Job) error // the runner's Start; a plain func value is the seam
}

func newBot(cfg Config, z *Zulip, start func(Job) error) *Bot {
	return &Bot{
		z:     z,
		start: start,
		// Zulip's call button posts "[Join video call.](<base>/<room>)" — the raw
		// content is enough, no markdown parsing needed. The room segment runs to
		// the first character markdown or prose can put after it: a narrower class
		// would truncate a legitimate name like "team.sync" and silently send the
		// recorder into a different room. "?" and "#" end it too, so a JWT or a
		// room password never reaches the job record or the logs.
		// A trailing slash on JITSI_BASE_URL would otherwise demand two of them.
		jitsiRe: regexp.MustCompile(regexp.QuoteMeta(strings.TrimRight(cfg.JitsiBaseURL, "/")) + "/[^\\s<>()\\[\\]{}\"'`?#|]+"),
	}
}

// Run drives the event loop until ctx ends. Only a failure to identify the bot
// is fatal — bad credentials should stop the service at startup rather than
// spin; everything after that is logged and retried.
func (b *Bot) Run(ctx context.Context) error {
	id, err := b.z.Me(ctx)
	if err != nil {
		return fmt.Errorf("zulip identity: %w", err)
	}
	b.botID = id
	slog.Info("zulip bot connected", "bot_user_id", id)

	var queueID string
	var lastEventID int64
	for ctx.Err() == nil {
		if queueID == "" {
			queueID, lastEventID, err = b.z.Register(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				slog.Error("zulip register failed", "err", err)
				pause(ctx, retryDelay)
				continue
			}
			slog.Info("zulip event queue registered", "last_event_id", lastEventID)
		}

		events, err := b.z.Events(ctx, queueID, lastEventID)
		switch {
		case errors.Is(err, errBadQueue):
			slog.Warn("zulip event queue expired, re-registering")
			queueID = ""
			continue
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("zulip events failed", "err", err)
			pause(ctx, retryDelay)
			continue
		}
		for _, ev := range events {
			if ev.ID > lastEventID {
				lastEventID = ev.ID
			}
			b.handle(ctx, ev)
		}
	}
	return nil
}

// handle acts on one event. Every failure is logged, never returned: one bad
// message must not take the loop down.
func (b *Bot) handle(ctx context.Context, ev Event) {
	switch {
	case ev.Type == "message" && ev.Message != nil:
		m := ev.Message
		if m.Type != "stream" || m.SenderID == b.botID || !b.jitsiRe.MatchString(m.Content) {
			return
		}
		// Offer the recording silently — a reaction, never a text message.
		if err := b.z.AddReaction(ctx, m.ID, micEmoji); err != nil {
			slog.Error("adding the microphone reaction failed", "message_id", m.ID, "err", err)
		}
	case ev.Type == "reaction" && ev.Op == "add" && ev.EmojiName == micEmoji && ev.UserID != b.botID:
		b.startJob(ctx, ev.MessageID)
	}
}

// startJob turns a microphone-reaction click into a recording job. The reaction
// event carries no content, so the message has to be fetched.
func (b *Bot) startJob(ctx context.Context, msgID int64) {
	m, err := b.z.GetMessage(ctx, msgID)
	if err != nil {
		slog.Error("fetching the reacted message failed", "message_id", msgID, "err", err)
		return
	}
	if m.Type != "stream" {
		return
	}
	// ponytail: sentence punctuation right after a pasted link is trimmed, so a
	// room literally named "standup." is unreachable from prose. Drop the trim if
	// anyone ever names one that.
	roomURL := strings.TrimRight(b.jitsiRe.FindString(m.Content), ".,;:!")
	if roomURL == "" || strings.HasSuffix(roomURL, "/") {
		return
	}
	job := Job{
		ID:        strconv.FormatInt(msgID, 10),
		MessageID: msgID,
		Stream:    string(m.DisplayRecipient),
		Topic:     m.Subject,
		JitsiURL:  roomURL,
	}
	// The "recording now" indicator goes on before the job starts, not after: the
	// runner clears it when the job ends, and a recorder that dies immediately can
	// clear it before an add issued afterwards would land — stranding a red dot on
	// a message whose recording already failed.
	addErr := b.z.AddReaction(ctx, msgID, recordingEmoji)
	switch err := b.start(job); {
	case errors.Is(err, ErrDuplicateJob):
		// Already recording: the indicator is already there, so addErr is the
		// expected "reaction already exists". A second click is a silent no-op.
	case err != nil:
		slog.Error("starting the recording failed", "job", job.ID, "err", err)
		if err := b.z.RemoveReaction(ctx, msgID, recordingEmoji); err != nil {
			slog.Error("removing the recording reaction failed", "message_id", msgID, "err", err)
		}
	case addErr != nil:
		slog.Error("adding the recording reaction failed", "message_id", msgID, "err", addErr)
	}
}

// pause sleeps for d, or returns early when ctx ends.
func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
