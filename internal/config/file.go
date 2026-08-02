package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// fileConfig is the JSON schema of the settings file written by the web UI.
// DBPath is deliberately absent: the database location is infrastructure,
// configured only via the DB_PATH environment variable.
type fileConfig struct {
	TelegramToken    string `json:"telegram_token"`
	AllowedChatID    int64  `json:"allowed_chat_id"`
	ProwlarrURL      string `json:"prowlarr_url"`
	ProwlarrAPIKey   string `json:"prowlarr_api_key"`
	TransmissionURL  string `json:"transmission_url"`
	TransmissionUser string `json:"transmission_user,omitempty"`
	TransmissionPass string `json:"transmission_pass,omitempty"`
	SubInterval      string `json:"sub_interval,omitempty"` // Go duration, e.g. "20m"
}

// LoadFrom loads configuration file-first: when the file at path exists it is
// the sole source for every user-config field (environment variables never
// fill per-field gaps), and when it is absent the environment is used exactly
// like Load. DBPath always comes from the DB_PATH environment variable.
// Missing required fields are reported as *ErrIncomplete so callers can start
// the settings page in setup mode instead of failing.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Load()
	}
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	cfg := &Config{
		TelegramToken:    fc.TelegramToken,
		AllowedChatID:    fc.AllowedChatID,
		ProwlarrURL:      fc.ProwlarrURL,
		ProwlarrAPIKey:   fc.ProwlarrAPIKey,
		TransmissionURL:  fc.TransmissionURL,
		TransmissionUser: fc.TransmissionUser,
		TransmissionPass: fc.TransmissionPass,
		SubInterval:      DefaultSubInterval,
	}
	if fc.SubInterval != "" {
		d, err := time.ParseDuration(fc.SubInterval)
		if err != nil {
			return nil, fmt.Errorf("config file %s: sub_interval must be a Go duration (e.g. 20m), got %q",
				path, fc.SubInterval)
		}
		cfg.SubInterval = d
	}

	cfg.DBPath = os.Getenv("DB_PATH")
	if cfg.DBPath == "" {
		cfg.DBPath = DefaultDBPath
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the user-config fields of c to path as JSON readable only by
// the owner (0600 — the file holds the bot token and other secrets). DBPath
// is not saved; see fileConfig.
func (c *Config) Save(path string) error {
	fc := fileConfig{
		TelegramToken:    c.TelegramToken,
		AllowedChatID:    c.AllowedChatID,
		ProwlarrURL:      c.ProwlarrURL,
		ProwlarrAPIKey:   c.ProwlarrAPIKey,
		TransmissionURL:  c.TransmissionURL,
		TransmissionUser: c.TransmissionUser,
		TransmissionPass: c.TransmissionPass,
		SubInterval:      c.SubInterval.String(),
	}
	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}
