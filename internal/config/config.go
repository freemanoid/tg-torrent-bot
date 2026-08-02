// Package config loads and validates bot configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
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
	AllowedChatID    int64         // ALLOWED_CHAT_ID: single-user allowlist
	ProwlarrURL      string        // PROWLARR_URL: e.g. http://umbrel.local:9696
	ProwlarrAPIKey   string        // PROWLARR_API_KEY
	TransmissionURL  string        // TRANSMISSION_URL: e.g. http://umbrel.local:9091
	TransmissionUser string        // TRANSMISSION_USER: optional RPC auth
	TransmissionPass string        // TRANSMISSION_PASS: optional RPC auth
	DBPath           string        // DB_PATH: SQLite location, default /data/bot.db
	SubInterval      time.Duration // SUB_INTERVAL: subscription tick interval, default 20m
}

// Load reads configuration from environment variables, applying defaults for
// optional values. An empty value is treated the same as an unset variable.
func Load() (*Config, error) {
	cfg := &Config{
		TransmissionUser: os.Getenv("TRANSMISSION_USER"),
		TransmissionPass: os.Getenv("TRANSMISSION_PASS"),
	}

	var err error
	if cfg.TelegramToken, err = requireEnv("TELEGRAM_TOKEN"); err != nil {
		return nil, err
	}
	chatID, err := requireEnv("ALLOWED_CHAT_ID")
	if err != nil {
		return nil, err
	}
	if cfg.AllowedChatID, err = strconv.ParseInt(chatID, 10, 64); err != nil {
		return nil, fmt.Errorf("ALLOWED_CHAT_ID must be an integer chat ID, got %q", chatID)
	}
	if cfg.AllowedChatID == 0 {
		// 0 is the "update carries no chat" sentinel in the allowlist
		// middleware; a real Telegram chat never has ID 0.
		return nil, fmt.Errorf("ALLOWED_CHAT_ID must be a real chat ID, got 0")
	}
	if cfg.ProwlarrURL, err = requireEnv("PROWLARR_URL"); err != nil {
		return nil, err
	}
	if cfg.ProwlarrAPIKey, err = requireEnv("PROWLARR_API_KEY"); err != nil {
		return nil, err
	}
	if cfg.TransmissionURL, err = requireEnv("TRANSMISSION_URL"); err != nil {
		return nil, err
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

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("missing required environment variable %s", name)
	}
	return v, nil
}
