package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// testOpts returns app options wired for tests: settings saves land in a
// config file inside a temp dir, the restart hook only counts calls instead
// of exiting, and the Telegram client skips getMe. The returned path and
// counter let tests assert on the save-and-restart flow.
func testOpts(t *testing.T, botOpts ...bot.Option) (opts appOpts, cfgPath string, restarts *int) {
	t.Helper()
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	restarts = new(int)
	opts = appOpts{
		store:   configFile(cfgPath),
		restart: func() { *restarts++ },
		log:     discardLog(),
		botOpts: append([]bot.Option{bot.WithSkipGetMe()}, botOpts...),
	}
	return opts, cfgPath, restarts
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

// freeAddr returns a loopback address that was free a moment ago. Racy in
// principle, fine on a test machine, and unlike an ephemeral port it leaves
// the test knowing where the settings server will land.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// awaitHealthz polls the settings server until it answers, proving it was
// started as part of Run.
func awaitHealthz(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("settings server at %s did not answer /healthz within 5s", addr)
}

// runInBackground starts a.Run and returns a channel carrying its result.
func runInBackground(a *app) (context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	return cancel, done
}

// awaitRun waits for Run to return and reports its error.
func awaitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after context cancel")
		return nil
	}
}

func TestNewAppWiresComponents(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")
	opts, _, _ := testOpts(t)

	a, err := newApp(cfg, opts)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()

	if a.st == nil || a.prowlarr == nil || a.trans == nil || a.bot == nil || a.engine == nil || a.watcher == nil {
		t.Fatalf("newApp left components unwired: %+v", a)
	}
	if a.web == nil {
		t.Error("newApp did not wire the settings server")
	}
	if a.setupMode() {
		t.Error("app with a complete config reports setup mode")
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		t.Errorf("database file not created at %s: %v", cfg.DBPath, err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// With no usable configuration the app must come up as the settings page
// alone: no database, no Telegram client, no background loops.
func TestNewAppSetupModeWiresWebOnly(t *testing.T) {
	dbDir := t.TempDir()
	t.Setenv("DB_PATH", filepath.Join(dbDir, "bot.db"))
	opts, _, _ := testOpts(t)

	a, err := newApp(nil, opts)
	if err != nil {
		t.Fatalf("newApp(nil): %v", err)
	}
	defer a.Close()

	if a.web == nil {
		t.Fatal("setup mode did not wire the settings server")
	}
	if !a.setupMode() {
		t.Error("app with no config does not report setup mode")
	}
	if a.st != nil || a.prowlarr != nil || a.trans != nil || a.bot != nil || a.engine != nil || a.watcher != nil {
		t.Errorf("setup mode wired bot components: %+v", a)
	}
	if entries, err := os.ReadDir(dbDir); err != nil {
		t.Fatalf("read dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("setup mode touched the database directory: %v", entries)
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close in setup mode: %v", err)
	}
}

func TestNewAppRejectsBadTransmissionURL(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")
	cfg.TransmissionURL = "ftp://not-http"
	opts, _, _ := testOpts(t)

	if _, err := newApp(cfg, opts); err == nil {
		t.Fatal("newApp: want error for non-http transmission URL, got nil")
	}
}

func TestNewAppRejectsBadProwlarrURL(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")
	cfg.ProwlarrURL = "umbrel.local:9696" // missing scheme
	opts, _, _ := testOpts(t)

	if _, err := newApp(cfg, opts); err == nil {
		t.Fatal("newApp: want error for schemeless prowlarr URL, got nil")
	}
}

func TestNewAppRejectsUnwritableDBPath(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:0")
	cfg.DBPath = filepath.Join(t.TempDir(), "missing-dir", "bot.db")
	opts, _, _ := testOpts(t)

	if _, err := newApp(cfg, opts); err == nil {
		t.Fatal("newApp: want error for unwritable DB path, got nil")
	}
}

func TestAppRunExitsCleanlyOnCancel(t *testing.T) {
	srv, hits := fakeBackend(t)
	cfg := testConfig(t, srv.URL)
	opts, _, _ := testOpts(t, bot.WithServerURL(srv.URL))

	a, err := newApp(cfg, opts)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()
	a.web.Addr = "127.0.0.1:0" // never bind the real settings port in tests

	cancel, done := runInBackground(a)

	// Wait until the loops demonstrably hit the backend, then shut down.
	awaitHit(t, hits)
	cancel()

	if err := awaitRun(t, done); err != nil {
		t.Errorf("Run returned %v, want nil on context cancel", err)
	}
}

// In full mode the settings server runs alongside the three loops, so the
// bot stays configurable while it works.
func TestAppRunServesSettingsAlongsideLoops(t *testing.T) {
	srv, hits := fakeBackend(t)
	cfg := testConfig(t, srv.URL)
	opts, _, _ := testOpts(t, bot.WithServerURL(srv.URL))

	a, err := newApp(cfg, opts)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()
	a.web.Addr = freeAddr(t)

	cancel, done := runInBackground(a)

	awaitHit(t, hits)           // the bot loops are running
	awaitHealthz(t, a.web.Addr) // and so is the settings server
	cancel()

	if err := awaitRun(t, done); err != nil {
		t.Errorf("Run returned %v, want nil on context cancel", err)
	}
}

// A settings server that cannot bind must not take the bot down: the loops
// are the app's actual job.
func TestAppRunSurvivesWebListenFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer occupied.Close()

	srv, hits := fakeBackend(t)
	cfg := testConfig(t, srv.URL)
	opts, _, _ := testOpts(t, bot.WithServerURL(srv.URL))

	a, err := newApp(cfg, opts)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()
	a.web.Addr = occupied.Addr().String()

	cancel, done := runInBackground(a)

	awaitHit(t, hits) // loops still running despite the dead settings server
	cancel()

	if err := awaitRun(t, done); err != nil {
		t.Errorf("Run returned %v, want nil even when the settings server cannot bind", err)
	}
}

// In setup mode the settings page is the whole app, so it serves the form
// and a failure to bind is fatal (the process has nothing else to do).
func TestAppRunSetupModeServesSettingsOnly(t *testing.T) {
	opts, _, _ := testOpts(t)
	a, err := newApp(nil, opts)
	if err != nil {
		t.Fatalf("newApp(nil): %v", err)
	}
	defer a.Close()
	a.web.Addr = freeAddr(t)

	cancel, done := runInBackground(a)

	awaitHealthz(t, a.web.Addr)
	resp, err := http.Get("http://" + a.web.Addr + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "Not configured yet") {
		t.Error("setup mode does not serve the setup form")
	}
	cancel()

	if err := awaitRun(t, done); err != nil {
		t.Errorf("Run returned %v, want nil on context cancel", err)
	}
}

func TestAppRunSetupModeFailsWhenWebCannotBind(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer occupied.Close()

	opts, _, _ := testOpts(t)
	a, err := newApp(nil, opts)
	if err != nil {
		t.Fatalf("newApp(nil): %v", err)
	}
	defer a.Close()
	a.web.Addr = occupied.Addr().String()

	if err := a.Run(context.Background()); err == nil {
		t.Error("Run in setup mode returned nil with no settings server, want error")
	}
}

// End-to-end for the seam Task 3 owns: a form submission is persisted to the
// config file through configFile and read back by the loader the next start
// uses, and the restart hook fires exactly once.
func TestSettingsSaveWritesConfigFileAndRestarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "bot.db")
	t.Setenv("DB_PATH", dbPath)

	opts, cfgPath, restarts := testOpts(t)
	a, err := newApp(nil, opts)
	if err != nil {
		t.Fatalf("newApp(nil): %v", err)
	}
	defer a.Close()

	form := url.Values{
		"telegram_token":    {"123456:test-token"},
		"allowed_chat_id":   {"42"},
		"prowlarr_url":      {"http://umbrel.local:9696"},
		"prowlarr_api_key":  {"prowlarr-key"},
		"transmission_url":  {"http://umbrel.local:9091"},
		"transmission_user": {"tuser"},
		"transmission_pass": {"tpass"},
		"sub_interval":      {"30m"},
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.web.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if *restarts != 1 {
		t.Errorf("restart hook called %d times, want 1", *restarts)
	}

	got, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatalf("LoadFrom(%s) after save: %v", cfgPath, err)
	}
	want := &config.Config{
		TelegramToken:    "123456:test-token",
		AllowedChatID:    42,
		ProwlarrURL:      "http://umbrel.local:9696",
		ProwlarrAPIKey:   "prowlarr-key",
		TransmissionURL:  "http://umbrel.local:9091",
		TransmissionUser: "tuser",
		TransmissionPass: "tpass",
		DBPath:           dbPath,
		SubInterval:      30 * time.Minute,
	}
	if *got != *want {
		t.Errorf("config reloaded from %s:\n got %+v\nwant %+v", cfgPath, *got, *want)
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
	opts, _, _ := testOpts(t, bot.WithServerURL(tgSrv.URL))

	a, err := newApp(cfg, opts)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer a.Close()
	a.web.Addr = "127.0.0.1:0"

	cancel, done := runInBackground(a)

	// The Telegram loop reaching its (healthy) fake proves Run survived the
	// dead Prowlarr/Transmission self-checks.
	awaitHit(t, tgHits)
	cancel()

	if err := awaitRun(t, done); err != nil {
		t.Errorf("Run returned %v, want nil even with dead backends", err)
	}
}

// The production restart hook must return immediately — the HTTP handler has
// to flush its response first — and only then exit non-zero so the container
// runtime restarts the bot with the saved configuration.
func TestRestarterExitsNonZeroAfterDelay(t *testing.T) {
	codes := make(chan int, 1)
	restart := restarter(discardLog(), 50*time.Millisecond, func(code int) { codes <- code })

	restart()
	select {
	case code := <-codes:
		t.Fatalf("restart hook exited synchronously with code %d, want a deferred exit", code)
	default:
	}

	select {
	case code := <-codes:
		if code != 1 {
			t.Errorf("exit code = %d, want 1 so the container runtime restarts the bot", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restart hook never exited")
	}
}

func TestConfigPath(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"unset falls back to the data volume", "", defaultConfigPath},
		{"CONFIG_PATH wins", "/somewhere/else/config.json", "/somewhere/else/config.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONFIG_PATH", tt.env)
			if got := configPath(); got != tt.want {
				t.Errorf("configPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
