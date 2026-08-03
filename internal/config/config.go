// Package config loads and validates bot configuration from the settings
// file written by the web UI (preferred) or from environment variables
// (fallback for headless installs).
package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Defaults for optional configuration values.
const (
	DefaultDBPath      = "/data/bot.db"
	DefaultSubInterval = 20 * time.Minute
)

// Config holds all runtime configuration for the bot.
type Config struct {
	TelegramToken    string        // TELEGRAM_TOKEN: bot token from @BotFather
	AllowedChatIDs   []int64       // ALLOWED_CHAT_ID: comma-separated chat allowlist
	ProwlarrURL      string        // PROWLARR_URL: e.g. http://umbrel.local:9696
	ProwlarrAPIKey   string        // PROWLARR_API_KEY
	TransmissionURL  string        // TRANSMISSION_URL: e.g. http://umbrel.local:9091
	TransmissionUser string        // TRANSMISSION_USER: optional RPC auth
	TransmissionPass string        // TRANSMISSION_PASS: optional RPC auth
	DBPath           string        // DB_PATH: SQLite location, default /data/bot.db
	SubInterval      time.Duration // SUB_INTERVAL: subscription tick interval, default 20m
}

// ErrIncomplete reports a configuration whose required fields are missing.
// Callers treat it as "run the settings page in setup mode" rather than a
// fatal error, so a fresh install can be configured from the browser.
type ErrIncomplete struct {
	// Missing lists the absent required fields, named after the source they
	// were loaded from: environment variable names for env loads, JSON field
	// names for file loads.
	Missing []string
}

func (e *ErrIncomplete) Error() string {
	return "incomplete configuration: missing " + strings.Join(e.Missing, ", ")
}

// Load reads configuration from environment variables, applying defaults for
// optional values. An empty value is treated the same as an unset variable.
// When required variables are missing it returns *ErrIncomplete listing all
// of them.
func Load() (*Config, error) {
	cfg := &Config{
		TelegramToken:    os.Getenv("TELEGRAM_TOKEN"),
		ProwlarrURL:      os.Getenv("PROWLARR_URL"),
		ProwlarrAPIKey:   os.Getenv("PROWLARR_API_KEY"),
		TransmissionURL:  os.Getenv("TRANSMISSION_URL"),
		TransmissionUser: os.Getenv("TRANSMISSION_USER"),
		TransmissionPass: os.Getenv("TRANSMISSION_PASS"),
	}
	chatID := os.Getenv("ALLOWED_CHAT_ID")

	var missing []string
	for _, v := range []struct{ name, value string }{
		{"TELEGRAM_TOKEN", cfg.TelegramToken},
		{"ALLOWED_CHAT_ID", chatID},
		{"PROWLARR_URL", cfg.ProwlarrURL},
		{"PROWLARR_API_KEY", cfg.ProwlarrAPIKey},
		{"TRANSMISSION_URL", cfg.TransmissionURL},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrIncomplete{Missing: missing}
	}

	var err error
	if cfg.AllowedChatIDs, err = ParseChatIDs(chatID); err != nil {
		return nil, fmt.Errorf("ALLOWED_CHAT_ID: %w", err)
	}
	if len(cfg.AllowedChatIDs) == 0 {
		return nil, &ErrIncomplete{Missing: []string{"ALLOWED_CHAT_ID"}}
	}

	cfg.DBPath = os.Getenv("DB_PATH")
	if cfg.DBPath == "" {
		cfg.DBPath = DefaultDBPath
	}

	cfg.SubInterval = DefaultSubInterval
	if v := os.Getenv("SUB_INTERVAL"); v != "" {
		if cfg.SubInterval, err = time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("SUB_INTERVAL must be a Go duration (e.g. 20m), got %q", v)
		}
		if cfg.SubInterval <= 0 {
			// A non-positive interval would panic time.NewTicker in the
			// subscription engine; there is no "disable" setting.
			return nil, fmt.Errorf("SUB_INTERVAL must be positive, got %q", v)
		}
	}

	return cfg, nil
}

// Validate checks the user-config fields (everything except DBPath, which is
// infrastructure). Missing required fields are reported as *ErrIncomplete
// using JSON field names; present-but-malformed values as plain errors. It
// backs both file loading and the settings form, so the two agree on what a
// complete configuration is.
func (c *Config) Validate() error {
	var missing []string
	for _, v := range []struct{ name, value string }{
		{"telegram_token", c.TelegramToken},
		{"allowed_chat_ids", FormatChatIDs(c.AllowedChatIDs)},
		{"prowlarr_url", c.ProwlarrURL},
		{"prowlarr_api_key", c.ProwlarrAPIKey},
		{"transmission_url", c.TransmissionURL},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return &ErrIncomplete{Missing: missing}
	}

	for _, id := range c.AllowedChatIDs {
		if id == 0 {
			// 0 is the "update carries no chat" sentinel in the allowlist
			// middleware; a real Telegram chat never has ID 0.
			return fmt.Errorf("allowed_chat_ids must be real chat IDs, got 0")
		}
	}
	if err := validateURL("prowlarr_url", c.ProwlarrURL); err != nil {
		return err
	}
	if err := validateURL("transmission_url", c.TransmissionURL); err != nil {
		return err
	}
	if c.SubInterval <= 0 {
		// A non-positive interval would panic time.NewTicker in the
		// subscription engine; there is no "disable" setting.
		return fmt.Errorf("sub_interval must be positive, got %v", c.SubInterval)
	}
	return nil
}

// ParseChatIDs reads a comma-separated chat allowlist, as written in
// ALLOWED_CHAT_ID or typed into the settings form. Blank entries are skipped
// so a stray trailing comma is not an error, and repeats are dropped so a
// chat listed twice is not notified twice. An empty input yields no IDs and
// no error — "absent" is Validate's business, not the parser's.
func ParseChatIDs(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("must be comma-separated integer chat IDs, got %q", part)
		}
		if id == 0 {
			// 0 is the "update carries no chat" sentinel in the allowlist
			// middleware; a real Telegram chat never has ID 0.
			return nil, fmt.Errorf("must be real chat IDs, got 0")
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// FormatChatIDs renders a chat allowlist the way ParseChatIDs reads it, which
// is also how the settings form shows it. An empty list yields an empty
// string, so the missing-field loop can treat it like any other absent value.
func FormatChatIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func validateURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http(s) URL, got %q", field, raw)
	}
	return nil
}
