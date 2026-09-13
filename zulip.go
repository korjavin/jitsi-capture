package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Zulip is a thin client over the handful of Zulip REST endpoints this service
// uses. Every call is form-encoded and authenticated with HTTP basic auth
// (bot email : API key).
type Zulip struct {
	site, email, key string
	http             *http.Client
}

// The runner only needs two of these methods; keep the shapes in sync.
var _ zulipAPI = (*Zulip)(nil)

// micEmoji is the reaction the bot offers on a call message; clicking it starts
// a recording. recordingEmoji (runner.go) is the "recording now" indicator.
const micEmoji = "studio_microphone"

// errBadQueue reports an expired event queue (Zulip code BAD_EVENT_QUEUE_ID).
// The event loop answers it by registering a fresh queue.
var errBadQueue = errors.New("zulip: bad event queue id")

func newZulip(cfg Config) *Zulip {
	return &Zulip{
		site:  cfg.ZulipSite,
		email: cfg.ZulipBotEmail,
		key:   cfg.ZulipBotAPIKey,
		// Long enough for an /events long-poll, which Zulip holds ~90 s.
		http: &http.Client{Timeout: 120 * time.Second},
	}
}

// Message is the part of a Zulip message the bot reads.
type Message struct {
	ID               int64      `json:"id"`
	Type             string     `json:"type"` // "stream" | "private"
	Content          string     `json:"content"`
	DisplayRecipient streamName `json:"display_recipient"`
	Subject          string     `json:"subject"` // the topic
	SenderID         int64      `json:"sender_id"`
}

// streamName is Zulip's display_recipient: the stream name for stream messages,
// but an array of users for DMs. A DM decodes to "" instead of failing the whole
// event batch — the bot ignores non-stream messages anyway.
type streamName string

func (s *streamName) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	*s = streamName(v)
	return nil
}

// Event is one entry from the Events API. Message events carry the message;
// reaction events carry the op/user/message/emoji fields at the top level.
type Event struct {
	ID        int64    `json:"id"`
	Type      string   `json:"type"`
	Op        string   `json:"op"`
	UserID    int64    `json:"user_id"`
	MessageID int64    `json:"message_id"`
	EmojiName string   `json:"emoji_name"`
	Message   *Message `json:"message"`
}

// Me returns the bot's own user id, used to ignore its own messages and
// reactions.
func (z *Zulip) Me(ctx context.Context) (int64, error) {
	var r struct {
		UserID int64 `json:"user_id"`
	}
	err := z.do(ctx, http.MethodGet, "/api/v1/users/me", nil, nil, &r)
	return r.UserID, err
}

// Register opens an event queue for message and reaction events.
// apply_markdown=false makes message events carry the raw content, so matching
// the Jitsi URL needs no second call.
func (z *Zulip) Register(ctx context.Context) (queueID string, lastEventID int64, err error) {
	form := url.Values{
		"event_types":    {`["message","reaction"]`},
		"apply_markdown": {"false"},
	}
	var r struct {
		QueueID     string `json:"queue_id"`
		LastEventID int64  `json:"last_event_id"`
	}
	err = z.do(ctx, http.MethodPost, "/api/v1/register", nil, form, &r)
	return r.QueueID, r.LastEventID, err
}

// Events long-polls for events after lastEventID. An expired queue comes back
// as errBadQueue.
func (z *Zulip) Events(ctx context.Context, queueID string, lastEventID int64) ([]Event, error) {
	q := url.Values{
		"queue_id":      {queueID},
		"last_event_id": {strconv.FormatInt(lastEventID, 10)},
	}
	var r struct {
		Events []Event `json:"events"`
	}
	err := z.do(ctx, http.MethodGet, "/api/v1/events", q, nil, &r)
	return r.Events, err
}

// GetMessage fetches one message. apply_markdown=false is a query parameter
// here, not a form field.
func (z *Zulip) GetMessage(ctx context.Context, id int64) (Message, error) {
	var r struct {
		Message Message `json:"message"`
	}
	err := z.do(ctx, http.MethodGet, messagePath(id), url.Values{"apply_markdown": {"false"}}, nil, &r)
	return r.Message, err
}

func (z *Zulip) AddReaction(ctx context.Context, msgID int64, emojiName string) error {
	return z.do(ctx, http.MethodPost, messagePath(msgID)+"/reactions", nil, url.Values{"emoji_name": {emojiName}}, nil)
}

func (z *Zulip) RemoveReaction(ctx context.Context, msgID int64, emojiName string) error {
	return z.do(ctx, http.MethodDelete, messagePath(msgID)+"/reactions", nil, url.Values{"emoji_name": {emojiName}}, nil)
}

func (z *Zulip) SendMessage(ctx context.Context, stream, topic, content string) error {
	return z.do(ctx, http.MethodPost, "/api/v1/messages", nil, url.Values{
		"type":    {"stream"},
		"to":      {stream},
		"topic":   {topic},
		"content": {content},
	}, nil)
}

func messagePath(id int64) string {
	return "/api/v1/messages/" + strconv.FormatInt(id, 10)
}

// do runs one authenticated request and decodes the body into out (nil to
// discard it). Zulip signals failure both by HTTP status and by
// "result":"error" in the body; either becomes an error carrying Zulip's own
// message — never the credentials.
func (z *Zulip) do(ctx context.Context, method, path string, query, form url.Values, out any) error {
	u := z.site + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(z.email, z.key)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := z.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var status struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
		Code   string `json:"code"`
	}
	_ = json.Unmarshal(b, &status) // a non-JSON body leaves it zero; the status check below still fires
	if status.Result == "error" {
		if status.Code == "BAD_EVENT_QUEUE_ID" { // only /events produces this
			return errBadQueue
		}
		return fmt.Errorf("zulip %s %s: %s", method, path, status.Msg)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("zulip %s %s: http %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}
