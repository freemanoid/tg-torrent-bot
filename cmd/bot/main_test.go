package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"

	"github.com/freemanoid/tg-torrent-bot/internal/config"
)

func discardLog() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// testConfig returns a config whose external endpoints all point at base
// (an httptest server) and whose database lives in a temp dir.
func testConfig(t *testing.T, base string) *config.Config {
	t.Helper()
	return &config.Config{
		TelegramToken:   "123:testtoken",
		AllowedChatID:   42,
		ProwlarrURL:     base,
		ProwlarrAPIKey:  "key",
		TransmissionURL: base,
		DBPath:          filepath.Join(t.TempDir(), "bot.db"),
		SubInterval:     time.Hour,
	}
}

// fakeBackend answers just enough for the app to start offline: Telegram
// Bot API calls, the Prowlarr health check, and (incorrectly, on purpose)
// the Transmission RPC endpoint. The returned channel signals every request
// served, so tests can wait until the loops are demonstrably running instead
// of sleeping.
func fakeBackend(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	hits := make(chan string, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- r.URL.Path:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":42}}}`))
		case r.URL.Path == "/api/v1/health":
			w.Write([]byte(`[]`))
		default: // setMyCommands, transmission RPC, ...
			w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// awaitHit blocks until the fake backend has served at least one request,
// proving the app's loops actually started.
func awaitHit(t *testing.T, hits <-chan string) {
	t.Helper()
	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the fake backend within 5s")
	}
}

func TestNewAppWiresComponents(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")

	a, err := newApp(cfg, discardLog(), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()

	if a.st == nil || a.prowlarr == nil || a.trans == nil || a.bot == nil || a.engine == nil || a.watcher == nil {
		t.Fatalf("newApp left components unwired: %+v", a)
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		t.Errorf("database file not created at %s: %v", cfg.DBPath, err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestNewAppRejectsBadTransmissionURL(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")
	cfg.TransmissionURL = "ftp://not-http"

	if _, err := newApp(cfg, discardLog(), bot.WithSkipGetMe()); err == nil {
		t.Fatal("newApp: want error for non-http transmission URL, got nil")
	}
}

func TestNewAppRejectsBadProwlarrURL(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")
	cfg.ProwlarrURL = "umbrel.local:9696" // missing scheme

	if _, err := newApp(cfg, discardLog(), bot.WithSkipGetMe()); err == nil {
		t.Fatal("newApp: want error for schemeless prowlarr URL, got nil")
	}
}

func TestNewAppRejectsUnwritableDBPath(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")
	cfg.DBPath = filepath.Join(t.TempDir(), "missing-dir", "bot.db")

	if _, err := newApp(cfg, discardLog(), bot.WithSkipGetMe()); err == nil {
		t.Fatal("newApp: want error for unwritable DB path, got nil")
	}
}

func TestAppRunExitsCleanlyOnCancel(t *testing.T) {
	srv, hits := fakeBackend(t)
	cfg := testConfig(t, srv.URL)

	a, err := newApp(cfg, discardLog(), bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Wait until the loops demonstrably hit the backend, then shut down.
	awaitHit(t, hits)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on context cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}

// The self-check must warn, never kill the app: here both Prowlarr and
// Transmission are unreachable (closed server) yet Run still starts and
// shuts down cleanly.
func TestAppRunSurvivesUnreachableBackends(t *testing.T) {
	tgSrv, tgHits := fakeBackend(t)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // connection refused from here on

	cfg := testConfig(t, dead.URL)
	a, err := newApp(cfg, discardLog(), bot.WithSkipGetMe(), bot.WithServerURL(tgSrv.URL))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// The Telegram loop reaching its (healthy) fake proves Run survived the
	// dead Prowlarr/Transmission self-checks.
	awaitHit(t, tgHits)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil even with dead backends", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}
