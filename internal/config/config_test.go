package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	if want := []int64{42}; !slices.Equal(cfg.AllowedChatIDs, want) {
		t.Errorf("AllowedChatIDs = %v, want %v", cfg.AllowedChatIDs, want)
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
	if want := []int64{-1001234567890}; !slices.Equal(cfg.AllowedChatIDs, want) {
		t.Errorf("AllowedChatIDs = %v, want %v", cfg.AllowedChatIDs, want)
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
	if want := []int64{99}; !slices.Equal(cfg.AllowedChatIDs, want) {
		t.Errorf("AllowedChatIDs = %v, want %v", cfg.AllowedChatIDs, want)
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
	if !slices.Contains(inc.Missing, "allowed_chat_ids") {
		t.Errorf("Missing = %v, want to contain allowed_chat_ids", inc.Missing)
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
		AllowedChatIDs:   []int64{99, -1001234567890},
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
	if _, ok := keys["allowed_chat_id"]; ok {
		t.Error("saved file contains the legacy allowed_chat_id key, want only allowed_chat_ids")
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() after Save() returned error: %v", err)
	}
	want := *saved
	want.DBPath = DefaultDBPath // DB_PATH env is blank in this test
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, want)
	}
}

// --- chat allowlist ---

func TestLoadMultipleChatIDs(t *testing.T) {
	setRequired(t)
	t.Setenv("ALLOWED_CHAT_ID", "42, -1001234567890 ,7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	want := []int64{42, -1001234567890, 7}
	if !slices.Equal(cfg.AllowedChatIDs, want) {
		t.Errorf("AllowedChatIDs = %v, want %v", cfg.AllowedChatIDs, want)
	}
}

func TestParseChatIDs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []int64
	}{
		{"single", "42", []int64{42}},
		{"several", "42,7", []int64{42, 7}},
		{"spaces around entries", " 42 , 7 ", []int64{42, 7}},
		{"trailing comma", "42,7,", []int64{42, 7}},
		{"blank entries skipped", "42,,7", []int64{42, 7}},
		// a chat listed twice would otherwise be notified twice
		{"repeats dropped", "42,7,42", []int64{42, 7}},
		{"empty", "", nil},
		{"only separators", " , ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseChatIDs(tt.raw)
			if err != nil {
				t.Fatalf("ParseChatIDs(%q) returned error: %v", tt.raw, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ParseChatIDs(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseChatIDsBad(t *testing.T) {
	// 0 is the middleware's "update carries no chat" sentinel, never a chat.
	for _, bad := range []string{"abc", "42,abc", "12.5", "12abc", "42;7", "0", "42,0"} {
		t.Run(bad, func(t *testing.T) {
			if _, err := ParseChatIDs(bad); err == nil {
				t.Errorf("ParseChatIDs(%q) succeeded, want error", bad)
			}
		})
	}
}

func TestFormatChatIDs(t *testing.T) {
	if got, want := FormatChatIDs([]int64{42, -7}), "42,-7"; got != want {
		t.Errorf("FormatChatIDs = %q, want %q", got, want)
	}
	if got := FormatChatIDs(nil); got != "" {
		t.Errorf("FormatChatIDs(nil) = %q, want empty", got)
	}
}

func TestValidateRejectsZeroChatID(t *testing.T) {
	// 0 is the middleware's "update carries no chat" sentinel: an allowlist
	// containing it would let every chat-less update through.
	cfg := &Config{
		TelegramToken:   "t",
		AllowedChatIDs:  []int64{42, 0},
		ProwlarrURL:     "http://prowlarr.local:9696",
		ProwlarrAPIKey:  "k",
		TransmissionURL: "http://transmission.local:9091",
		SubInterval:     DefaultSubInterval,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted chat ID 0, want error")
	}
	var inc *ErrIncomplete
	if errors.As(err, &inc) {
		t.Fatalf("Validate() error = %v, want a plain validation error, not ErrIncomplete", err)
	}
	if !strings.Contains(err.Error(), "allowed_chat_ids") {
		t.Errorf("error %q does not mention allowed_chat_ids", err)
	}
}

// --- legacy single-chat config file ---

func TestLoadFromLegacyChatIDKey(t *testing.T) {
	// A config file written before the allowlist became a list must keep its
	// chat across the upgrade, or every existing install boots into setup mode.
	clearRequiredEnv(t)
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
	if want := []int64{99}; !slices.Equal(cfg.AllowedChatIDs, want) {
		t.Errorf("AllowedChatIDs = %v, want %v", cfg.AllowedChatIDs, want)
	}
}

func TestLoadFromPluralChatIDsWinsOverLegacy(t *testing.T) {
	// Both keys present (a file saved by an old version, then edited by hand):
	// the current key is the one that counts.
	clearRequiredEnv(t)
	path := writeConfigFile(t, `{
		"telegram_token": "654321:file-token",
		"allowed_chat_id": 99,
		"allowed_chat_ids": [7, 8],
		"prowlarr_url": "http://prowlarr.local:9696",
		"prowlarr_api_key": "file-prowlarr-key",
		"transmission_url": "http://transmission.local:9091"
	}`)

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() returned error: %v", err)
	}
	if want := []int64{7, 8}; !slices.Equal(cfg.AllowedChatIDs, want) {
		t.Errorf("AllowedChatIDs = %v, want %v", cfg.AllowedChatIDs, want)
	}
}

func TestLoadFromFileMultipleChatIDs(t *testing.T) {
	clearRequiredEnv(t)
	path := writeConfigFile(t, `{
		"telegram_token": "654321:file-token",
		"allowed_chat_ids": [99, -1001234567890],
		"prowlarr_url": "http://prowlarr.local:9696",
		"prowlarr_api_key": "file-prowlarr-key",
		"transmission_url": "http://transmission.local:9091"
	}`)

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() returned error: %v", err)
	}
	if want := []int64{99, -1001234567890}; !slices.Equal(cfg.AllowedChatIDs, want) {
		t.Errorf("AllowedChatIDs = %v, want %v", cfg.AllowedChatIDs, want)
	}
}

func TestLoadFromFileEmptyChatIDList(t *testing.T) {
	// An explicit empty list is as incomplete as no key at all.
	clearRequiredEnv(t)
	path := writeConfigFile(t, `{
		"telegram_token": "654321:file-token",
		"allowed_chat_ids": [],
		"prowlarr_url": "http://prowlarr.local:9696",
		"prowlarr_api_key": "file-prowlarr-key",
		"transmission_url": "http://transmission.local:9091"
	}`)

	_, err := LoadFrom(path)
	var inc *ErrIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("LoadFrom() error = %v, want *ErrIncomplete", err)
	}
	if !slices.Contains(inc.Missing, "allowed_chat_ids") {
		t.Errorf("Missing = %v, want to contain allowed_chat_ids", inc.Missing)
	}
}
