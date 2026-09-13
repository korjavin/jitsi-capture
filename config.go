package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the whole runtime configuration. config.go is the single reader of
// os.Getenv — compose passes the values via env_file, so there are no flags and
// no dotenv loading.
type Config struct {
	ZulipSite      string // no trailing slash
	ZulipBotEmail  string
	ZulipBotAPIKey string

	JitsiBaseURL   string
	DataDir        string // container path; jobs live in DataDir/jobs/<id>/
	HostDataDir    string // host path of the bind mount, used for audio_path in the webhook
	RecorderPath   string
	BotDisplayName string

	JoinTimeoutS       int
	MaxDurationS       int
	EmptyGraceS        int
	MinRecordingS      int
	AudioRetentionDays int

	WebhookURL    string // empty = webhook disabled
	WebhookSecret string // required when WebhookURL is set; also verifies POST /notify

	ListenAddr string
	PublicURL  string // callback_url = PublicURL + "/notify"
	LogLevel   string
}

// loadConfig reads the environment. It always returns a Config with defaults
// applied so the caller can set up logging from it even when err is non-nil.
// Errors name the offending variables only — never their values.
func loadConfig() (Config, error) {
	var missing []string
	var errs []error

	req := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}
	str := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	num := func(name string, def int) int {
		v := os.Getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s must be an integer", name))
			return def
		}
		return n
	}
	// Go evaluates function calls in a composite literal left to right, so the
	// missing-variable list comes out in the order below.
	c := Config{
		ZulipSite:      strings.TrimRight(req("ZULIP_SITE"), "/"),
		ZulipBotEmail:  req("ZULIP_BOT_EMAIL"),
		ZulipBotAPIKey: req("ZULIP_BOT_API_KEY"),

		JitsiBaseURL:   str("JITSI_BASE_URL", "https://meet.jit.si"),
		DataDir:        str("DATA_DIR", "/data"),
		HostDataDir:    os.Getenv("HOST_DATA_DIR"),
		RecorderPath:   str("RECORDER_PATH", "recorder/record.js"),
		BotDisplayName: str("BOT_DISPLAY_NAME", "NoteTaker"),

		JoinTimeoutS:       num("JOIN_TIMEOUT_S", 600),
		MaxDurationS:       num("MAX_DURATION_S", 14400),
		EmptyGraceS:        num("EMPTY_GRACE_S", 60),
		MinRecordingS:      num("MIN_RECORDING_S", 15),
		AudioRetentionDays: num("AUDIO_RETENTION_DAYS", 7),

		WebhookURL:    strings.TrimRight(os.Getenv("WEBHOOK_URL"), "/"),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),

		ListenAddr: str("LISTEN_ADDR", ":8080"),
		PublicURL:  strings.TrimRight(str("PUBLIC_URL", "http://localhost:8080"), "/"),
		LogLevel:   str("LOG_LEVEL", "INFO"),
	}
	if c.HostDataDir == "" {
		c.HostDataDir = c.DataDir
	}
	if c.WebhookURL != "" && c.WebhookSecret == "" {
		missing = append(missing, "WEBHOOK_SECRET")
	}
	if len(missing) > 0 {
		errs = append([]error{fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))}, errs...)
	}
	return c, errors.Join(errs...)
}
