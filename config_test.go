package main

import (
	"strings"
	"testing"
)

// setRequired sets the three always-required variables and clears every other
// variable loadConfig reads, so each test starts from a known environment.
func setRequired(t *testing.T) {
	t.Helper()
	for _, n := range []string{
		"JITSI_BASE_URL", "DATA_DIR", "HOST_DATA_DIR", "RECORDER_PATH", "BOT_DISPLAY_NAME",
		"JOIN_TIMEOUT_S", "MAX_DURATION_S", "EMPTY_GRACE_S", "MIN_RECORDING_S",
		"AUDIO_RETENTION_DAYS", "WEBHOOK_URL", "WEBHOOK_SECRET", "LISTEN_ADDR",
		"PUBLIC_URL", "LOG_LEVEL",
	} {
		t.Setenv(n, "")
	}
	t.Setenv("ZULIP_SITE", "https://zulip.example.com")
	t.Setenv("ZULIP_BOT_EMAIL", "bot@example.com")
	t.Setenv("ZULIP_BOT_API_KEY", "test-key")
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequired(t)

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"JitsiBaseURL", c.JitsiBaseURL, "https://meet.jit.si"},
		{"DataDir", c.DataDir, "/data"},
		{"HostDataDir", c.HostDataDir, "/data"},
		{"RecorderPath", c.RecorderPath, "recorder/record.js"},
		{"BotDisplayName", c.BotDisplayName, "NoteTaker"},
		{"ListenAddr", c.ListenAddr, ":8080"},
		{"PublicURL", c.PublicURL, "http://localhost:8080"},
		{"LogLevel", c.LogLevel, "INFO"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"JoinTimeoutS", c.JoinTimeoutS, 600},
		{"MaxDurationS", c.MaxDurationS, 14400},
		{"EmptyGraceS", c.EmptyGraceS, 60},
		{"MinRecordingS", c.MinRecordingS, 15},
		{"AudioRetentionDays", c.AudioRetentionDays, 7},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	if c.WebhookURL != "" {
		t.Errorf("WebhookURL = %q, want empty", c.WebhookURL)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	setRequired(t)
	t.Setenv("ZULIP_SITE", "")
	t.Setenv("ZULIP_BOT_API_KEY", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig: want error, got nil")
	}
	want := "missing required environment variables: ZULIP_SITE, ZULIP_BOT_API_KEY"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "ZULIP_BOT_EMAIL") {
		t.Errorf("error names a variable that was set: %q", err)
	}
}

func TestLoadConfigWebhookSecretRequiredWithURL(t *testing.T) {
	setRequired(t)
	t.Setenv("WEBHOOK_URL", "https://hook.example.com/in/")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig: want error, got nil")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_SECRET") {
		t.Fatalf("error = %q, want it to name WEBHOOK_SECRET", err)
	}

	t.Setenv("WEBHOOK_SECRET", "s3cret")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.WebhookURL != "https://hook.example.com/in" {
		t.Errorf("WebhookURL = %q, want the trailing slash trimmed", c.WebhookURL)
	}
}

func TestLoadConfigBadInt(t *testing.T) {
	setRequired(t)
	t.Setenv("JOIN_TIMEOUT_S", "ten minutes")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig: want error, got nil")
	}
	if !strings.Contains(err.Error(), "JOIN_TIMEOUT_S") {
		t.Fatalf("error = %q, want it to name JOIN_TIMEOUT_S", err)
	}
	if strings.Contains(err.Error(), "ten minutes") {
		t.Errorf("error leaks the value: %q", err)
	}
}

func TestLoadConfigTrimsAndOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("ZULIP_SITE", "https://zulip.example.com/")
	t.Setenv("PUBLIC_URL", "https://capture.example.com/")
	t.Setenv("DATA_DIR", "/srv/data")
	t.Setenv("MIN_RECORDING_S", "30")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.ZulipSite != "https://zulip.example.com" {
		t.Errorf("ZulipSite = %q, want the trailing slash trimmed", c.ZulipSite)
	}
	if c.PublicURL != "https://capture.example.com" {
		t.Errorf("PublicURL = %q, want the trailing slash trimmed", c.PublicURL)
	}
	if c.HostDataDir != "/srv/data" {
		t.Errorf("HostDataDir = %q, want it to default to DATA_DIR", c.HostDataDir)
	}
	if c.MinRecordingS != 30 {
		t.Errorf("MinRecordingS = %d, want 30", c.MinRecordingS)
	}
}
