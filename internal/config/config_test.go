package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// --- file-backed config: LoadFrom / Save / ErrIncomplete ---

// clearRequiredEnv blanks every required env var so tests can simulate a
// fresh install with no environment configuration.
func clearRequiredEnv(t *testing.T) {
	t.Helper()
	setRequired(t) // also blanks the optional vars
	for _, name := range []string{
		"TELEGRAM_TOKEN", "ALLOWED_CHAT_ID", "PROWLARR_URL",
		"PROWLARR_API_KEY", "TRANSMISSION_URL",
	} {
		t.Setenv(name, "")
	}
}

// writeConfigFile writes content to a fresh temp config path and returns it.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

const completeFileJSON = `{
	"telegram_token": "654321:file-token",
	"allowed_chat_id": 99,
	"prowlarr_url": "http://prowlarr.local:9696",
	"prowlarr_api_key": "file-prowlarr-key",
	"transmission_url": "http://transmission.local:9091",
	"transmission_user": "fuser",
	"transmission_pass": "fpass",
	"sub_interval": "45m"
}`

func TestLoadFromFileOverridesEnv(t *testing.T) {
	// env is fully set with different values; the file must win for every
	// user-config field, while DB_PATH stays env-driven.
	setRequired(t)
	t.Setenv("TRANSMISSION_USER", "envuser")
	t.Setenv("TRANSMISSION_PASS", "envpass")
	t.Setenv("SUB_INTERVAL", "1h")
	t.Setenv("DB_PATH", "/tmp/env.db")
	path := writeConfigFile(t, completeFileJSON)

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() returned error: %v", err)
	}

	if cfg.TelegramToken != "654321:file-token" {
		t.Errorf("TelegramToken = %q, want file value", cfg.TelegramToken)
	}
	if cfg.AllowedChatID != 99 {
		t.Errorf("AllowedChatID = %d, want 99", cfg.AllowedChatID)
	}
	if cfg.ProwlarrURL != "http://prowlarr.local:9696" {
		t.Errorf("ProwlarrURL = %q, want file value", cfg.ProwlarrURL)
	}
	if cfg.ProwlarrAPIKey != "file-prowlarr-key" {
		t.Errorf("ProwlarrAPIKey = %q, want file value", cfg.ProwlarrAPIKey)
	}
	if cfg.TransmissionURL != "http://transmission.local:9091" {
		t.Errorf("TransmissionURL = %q, want file value", cfg.TransmissionURL)
	}
	if cfg.TransmissionUser != "fuser" {
		t.Errorf("TransmissionUser = %q, want fuser", cfg.TransmissionUser)
	}
	if cfg.TransmissionPass != "fpass" {
		t.Errorf("TransmissionPass = %q, want fpass", cfg.TransmissionPass)
	}
	if cfg.SubInterval != 45*time.Minute {
		t.Errorf("SubInterval = %v, want 45m", cfg.SubInterval)
	}
	if cfg.DBPath != "/tmp/env.db" {
		t.Errorf("DBPath = %q, want env value /tmp/env.db", cfg.DBPath)
	}
}

func TestLoadFromFileIsSoleSource(t *testing.T) {
	// A present file is the sole source for user-config fields: env must not
	// fill per-field gaps. Missing optional fields get defaults, not env.
	setRequired(t)
	t.Setenv("TRANSMISSION_USER", "envuser")
	t.Setenv("SUB_INTERVAL", "1h")
	path := writeConfigFile(t, `{
		"telegram_token": "654321:file-token",
		"allowed_chat_id": 99,
		"prowlarr_url": "http://prowlarr.local:9696",
		"prowlarr_api_key": "file-prowlarr-key",
		"transmission_url": "http://transmission.local:9091"
	}`)

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() returned error: %v", err)
	}
	if cfg.SubInterval != DefaultSubInterval {
		t.Errorf("SubInterval = %v, want default %v (not env)", cfg.SubInterval, DefaultSubInterval)
	}
	if cfg.TransmissionUser != "" || cfg.TransmissionPass != "" {
		t.Errorf("Transmission credentials = %q/%q, want empty (not env)",
			cfg.TransmissionUser, cfg.TransmissionPass)
	}
}

func TestLoadFromFileIncomplete(t *testing.T) {
	// Required fields missing from the file are reported as ErrIncomplete
	// even when env could fill them: no per-field fallback.
	setRequired(t)
	path := writeConfigFile(t, `{
		"telegram_token": "654321:file-token",
		"allowed_chat_id": 99
	}`)

	_, err := LoadFrom(path)
	var inc *ErrIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("LoadFrom() error = %v, want *ErrIncomplete", err)
	}
	want := []string{"prowlarr_url", "prowlarr_api_key", "transmission_url"}
	if !slices.Equal(inc.Missing, want) {
		t.Errorf("Missing = %v, want %v", inc.Missing, want)
	}
}

func TestLoadFromFileZeroChatID(t *testing.T) {
	// chat id 0 in the file is indistinguishable from absent — reported missing
	setRequired(t)
	path := writeConfigFile(t, `{
		"telegram_token": "654321:file-token",
		"allowed_chat_id": 0,
		"prowlarr_url": "http://prowlarr.local:9696",
		"prowlarr_api_key": "file-prowlarr-key",
		"transmission_url": "http://transmission.local:9091"
	}`)

	_, err := LoadFrom(path)
	var inc *ErrIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("LoadFrom() error = %v, want *ErrIncomplete", err)
	}
	if !slices.Contains(inc.Missing, "allowed_chat_id") {
		t.Errorf("Missing = %v, want to contain allowed_chat_id", inc.Missing)
	}
}

func TestLoadFromFileBadValues(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		mention string
	}{
		{
			name: "bad sub_interval",
			json: `{"telegram_token":"t","allowed_chat_id":99,"prowlarr_url":"http://prowlarr.local:9696",
				"prowlarr_api_key":"k","transmission_url":"http://transmission.local:9091","sub_interval":"soon"}`,
			mention: "sub_interval",
		},
		{
			name: "negative sub_interval",
			json: `{"telegram_token":"t","allowed_chat_id":99,"prowlarr_url":"http://prowlarr.local:9696",
				"prowlarr_api_key":"k","transmission_url":"http://transmission.local:9091","sub_interval":"-5m"}`,
			mention: "sub_interval",
		},
		{
			name: "bad prowlarr_url",
			json: `{"telegram_token":"t","allowed_chat_id":99,"prowlarr_url":"not a url",
				"prowlarr_api_key":"k","transmission_url":"http://transmission.local:9091"}`,
			mention: "prowlarr_url",
		},
		{
			name: "schemeless transmission_url",
			json: `{"telegram_token":"t","allowed_chat_id":99,"prowlarr_url":"http://prowlarr.local:9696",
				"prowlarr_api_key":"k","transmission_url":"transmission.local:9091"}`,
			mention: "transmission_url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequired(t)
			path := writeConfigFile(t, tt.json)

			_, err := LoadFrom(path)
			if err == nil {
				t.Fatalf("LoadFrom() succeeded, want error mentioning %s", tt.mention)
			}
			var inc *ErrIncomplete
			if errors.As(err, &inc) {
				t.Fatalf("LoadFrom() error = %v, want a plain validation error, not ErrIncomplete", err)
			}
			if !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("error %q does not mention %s", err, tt.mention)
			}
		})
	}
}

func TestLoadFromMalformedFile(t *testing.T) {
	setRequired(t)
	path := writeConfigFile(t, `{not json`)

	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("LoadFrom() succeeded on malformed JSON, want error")
	}
	var inc *ErrIncomplete
	if errors.As(err, &inc) {
		t.Errorf("malformed file reported as ErrIncomplete (%v), want a parse error", err)
	}
}

func TestLoadFromEnvFallback(t *testing.T) {
	// no file at the path → env is used exactly like Load()
	setRequired(t)
	t.Setenv("SUB_INTERVAL", "5m30s")

	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("LoadFrom() returned error: %v", err)
	}
	if cfg.TelegramToken != "123456:test-token" {
		t.Errorf("TelegramToken = %q, want env value", cfg.TelegramToken)
	}
	if cfg.SubInterval != 5*time.Minute+30*time.Second {
		t.Errorf("SubInterval = %v, want 5m30s", cfg.SubInterval)
	}
}

func TestLoadFromUnconfigured(t *testing.T) {
	// no file, no env → ErrIncomplete listing every required env var
	clearRequiredEnv(t)

	_, err := LoadFrom(filepath.Join(t.TempDir(), "missing.json"))
	var inc *ErrIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("LoadFrom() error = %v, want *ErrIncomplete", err)
	}
	want := []string{
		"TELEGRAM_TOKEN", "ALLOWED_CHAT_ID", "PROWLARR_URL",
		"PROWLARR_API_KEY", "TRANSMISSION_URL",
	}
	if !slices.Equal(inc.Missing, want) {
		t.Errorf("Missing = %v, want %v", inc.Missing, want)
	}
}

func TestLoadIncompleteTyped(t *testing.T) {
	// env loading reports missing vars as ErrIncomplete too, so callers can
	// switch to setup mode with errors.As regardless of the config source
	setRequired(t)
	t.Setenv("TELEGRAM_TOKEN", "")
	t.Setenv("PROWLARR_URL", "")

	_, err := Load()
	var inc *ErrIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("Load() error = %v, want *ErrIncomplete", err)
	}
	want := []string{"TELEGRAM_TOKEN", "PROWLARR_URL"}
	if !slices.Equal(inc.Missing, want) {
		t.Errorf("Missing = %v, want %v", inc.Missing, want)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	// env holds different values to prove LoadFrom reads the saved file
	setRequired(t)

	saved := &Config{
		TelegramToken:    "654321:file-token",
		AllowedChatID:    99,
		ProwlarrURL:      "http://prowlarr.local:9696",
		ProwlarrAPIKey:   "file-prowlarr-key",
		TransmissionURL:  "http://transmission.local:9091",
		TransmissionUser: "fuser",
		TransmissionPass: "fpass",
		DBPath:           "/tmp/must-not-persist.db",
		SubInterval:      45 * time.Minute,
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := saved.Save(path); err != nil {
		t.Fatalf("Save() returned error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if strings.Contains(string(raw), "must-not-persist") {
		t.Error("DBPath leaked into the config file; it is env-only infrastructure")
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	if _, ok := keys["db_path"]; ok {
		t.Error("saved file contains db_path key, want it omitted")
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() after Save() returned error: %v", err)
	}
	want := *saved
	want.DBPath = DefaultDBPath // DB_PATH env is blank in this test
	if *got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, want)
	}
}
