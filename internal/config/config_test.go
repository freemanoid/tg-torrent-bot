package config

import (
	"strings"
	"testing"
	"time"
)

// setRequired sets every required env var to a known-good value.
// Individual tests override or blank out vars as needed.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("TELEGRAM_TOKEN", "123456:test-token")
	t.Setenv("ALLOWED_CHAT_ID", "42")
	t.Setenv("PROWLARR_URL", "http://umbrel.local:9696")
	t.Setenv("PROWLARR_API_KEY", "prowlarr-key")
	t.Setenv("TRANSMISSION_URL", "http://umbrel.local:9091")
	// make sure optional vars from the outer environment don't leak in
	t.Setenv("TRANSMISSION_USER", "")
	t.Setenv("TRANSMISSION_PASS", "")
	t.Setenv("DB_PATH", "")
	t.Setenv("SUB_INTERVAL", "")
}

func TestLoadAllVars(t *testing.T) {
	setRequired(t)
	t.Setenv("TRANSMISSION_USER", "tuser")
	t.Setenv("TRANSMISSION_PASS", "tpass")
	t.Setenv("DB_PATH", "/tmp/custom.db")
	t.Setenv("SUB_INTERVAL", "5m30s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.TelegramToken != "123456:test-token" {
		t.Errorf("TelegramToken = %q", cfg.TelegramToken)
	}
	if cfg.AllowedChatID != 42 {
		t.Errorf("AllowedChatID = %d, want 42", cfg.AllowedChatID)
	}
	if cfg.ProwlarrURL != "http://umbrel.local:9696" {
		t.Errorf("ProwlarrURL = %q", cfg.ProwlarrURL)
	}
	if cfg.ProwlarrAPIKey != "prowlarr-key" {
		t.Errorf("ProwlarrAPIKey = %q", cfg.ProwlarrAPIKey)
	}
	if cfg.TransmissionURL != "http://umbrel.local:9091" {
		t.Errorf("TransmissionURL = %q", cfg.TransmissionURL)
	}
	if cfg.TransmissionUser != "tuser" {
		t.Errorf("TransmissionUser = %q, want tuser", cfg.TransmissionUser)
	}
	if cfg.TransmissionPass != "tpass" {
		t.Errorf("TransmissionPass = %q, want tpass", cfg.TransmissionPass)
	}
	if cfg.DBPath != "/tmp/custom.db" {
		t.Errorf("DBPath = %q, want /tmp/custom.db", cfg.DBPath)
	}
	if cfg.SubInterval != 5*time.Minute+30*time.Second {
		t.Errorf("SubInterval = %v, want 5m30s", cfg.SubInterval)
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.DBPath != "/data/bot.db" {
		t.Errorf("DBPath default = %q, want /data/bot.db", cfg.DBPath)
	}
	if cfg.SubInterval != 20*time.Minute {
		t.Errorf("SubInterval default = %v, want 20m", cfg.SubInterval)
	}
	if cfg.TransmissionUser != "" || cfg.TransmissionPass != "" {
		t.Errorf("Transmission credentials should default to empty, got %q/%q",
			cfg.TransmissionUser, cfg.TransmissionPass)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	required := []string{
		"TELEGRAM_TOKEN",
		"ALLOWED_CHAT_ID",
		"PROWLARR_URL",
		"PROWLARR_API_KEY",
		"TRANSMISSION_URL",
	}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(name, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded with %s unset, want error", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not mention missing var %s", err, name)
			}
		})
	}
}

func TestLoadBadChatID(t *testing.T) {
	for _, bad := range []string{"abc", "12.5", "12abc"} {
		t.Run(bad, func(t *testing.T) {
			setRequired(t)
			t.Setenv("ALLOWED_CHAT_ID", bad)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded with ALLOWED_CHAT_ID=%q, want error", bad)
			}
			if !strings.Contains(err.Error(), "ALLOWED_CHAT_ID") {
				t.Errorf("error %q does not mention ALLOWED_CHAT_ID", err)
			}
		})
	}
}

func TestLoadNegativeChatID(t *testing.T) {
	// group chats have negative IDs — must be accepted
	setRequired(t)
	t.Setenv("ALLOWED_CHAT_ID", "-1001234567890")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.AllowedChatID != -1001234567890 {
		t.Errorf("AllowedChatID = %d, want -1001234567890", cfg.AllowedChatID)
	}
}

func TestLoadZeroChatID(t *testing.T) {
	// 0 is the middleware's "update carries no chat" sentinel — must be rejected
	setRequired(t)
	t.Setenv("ALLOWED_CHAT_ID", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with ALLOWED_CHAT_ID=0, want error")
	}
	if !strings.Contains(err.Error(), "ALLOWED_CHAT_ID") {
		t.Errorf("error %q does not mention ALLOWED_CHAT_ID", err)
	}
}

func TestLoadBadSubInterval(t *testing.T) {
	// non-positive durations would panic time.NewTicker in the engine
	for _, bad := range []string{"twenty minutes", "0s", "0", "-5m"} {
		t.Run(bad, func(t *testing.T) {
			setRequired(t)
			t.Setenv("SUB_INTERVAL", bad)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() succeeded with SUB_INTERVAL=%q, want error", bad)
			}
			if !strings.Contains(err.Error(), "SUB_INTERVAL") {
				t.Errorf("error %q does not mention SUB_INTERVAL", err)
			}
		})
	}
}
