package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/config"
)

// fakeStore records saved configs and can be told to fail.
type fakeStore struct {
	saved []*config.Config
	err   error
}

func (f *fakeStore) Save(cfg *config.Config) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, cfg)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() *config.Config {
	return &config.Config{
		TelegramToken:    "123456:test-token",
		AllowedChatID:    42,
		ProwlarrURL:      "http://umbrel.local:9696",
		ProwlarrAPIKey:   "prowlarr-key",
		TransmissionURL:  "http://umbrel.local:9091",
		TransmissionUser: "tuser",
		TransmissionPass: "tpass",
		DBPath:           "/data/bot.db",
		SubInterval:      45 * time.Minute,
	}
}

// newTestServer wires a Server around fakes and returns them all.
func newTestServer(cfg *config.Config) (*Server, *fakeStore, *int) {
	store := &fakeStore{}
	restarts := 0
	s := New(cfg, store, func() { restarts++ }, testLogger())
	return s, store, &restarts
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func postForm(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// validForm returns a complete, valid settings submission.
func validForm() url.Values {
	return url.Values{
		"telegram_token":    {"654321:new-token"},
		"allowed_chat_id":   {"42"},
		"prowlarr_url":      {"http://umbrel.local:9696"},
		"prowlarr_api_key":  {"new-prowlarr-key"},
		"transmission_url":  {"http://umbrel.local:9091"},
		"transmission_user": {"tuser"},
		"transmission_pass": {"new-tpass"},
		"sub_interval":      {"30m"},
	}
}

func TestHealthz(t *testing.T) {
	s, _, _ := newTestServer(testConfig())

	rec := get(t, s, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("GET /healthz body = %q, want it to contain ok", rec.Body.String())
	}
}

func TestGetFormPrefilled(t *testing.T) {
	s, _, _ := newTestServer(testConfig())

	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`value="42"`,               // allowed chat id
		"http://umbrel.local:9696", // prowlarr url
		"http://umbrel.local:9091", // transmission url
		`value="tuser"`,            // transmission user (not a secret)
		`value="45m"`,              // sub interval, trimmed
		"Leave blank to keep",      // secret semantics hint
	} {
		if !strings.Contains(body, want) {
			t.Errorf("form does not contain %q", want)
		}
	}
	for _, secret := range []string{"123456:test-token", "prowlarr-key", "tpass"} {
		if strings.Contains(body, secret) {
			t.Errorf("form echoes secret %q into HTML", secret)
		}
	}
	if strings.Contains(body, "Not configured yet") {
		t.Error("configured server shows the setup banner")
	}
}

func TestGetFormUnconfigured(t *testing.T) {
	s, _, _ := newTestServer(nil)

	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Not configured yet") {
		t.Error("unconfigured server does not show the setup banner")
	}
	if !strings.Contains(body, `value="20m"`) {
		t.Error("unconfigured form does not prefill the default sub interval")
	}
	if strings.Contains(body, "Leave blank to keep") {
		t.Error("unconfigured form shows keep-current hint with no secrets to keep")
	}
}

func TestPostValidSavesAndRestarts(t *testing.T) {
	s, store, restarts := newTestServer(testConfig())

	rec := postForm(t, s, validForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Restarting") {
		t.Errorf("POST / body = %q, want a restarting page", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "654321:new-token") {
		t.Error("restarting page echoes the submitted token")
	}
	if *restarts != 1 {
		t.Fatalf("restart called %d times, want 1", *restarts)
	}
	if len(store.saved) != 1 {
		t.Fatalf("store.Save called %d times, want 1", len(store.saved))
	}

	got := store.saved[0]
	want := &config.Config{
		TelegramToken:    "654321:new-token",
		AllowedChatID:    42,
		ProwlarrURL:      "http://umbrel.local:9696",
		ProwlarrAPIKey:   "new-prowlarr-key",
		TransmissionURL:  "http://umbrel.local:9091",
		TransmissionUser: "tuser",
		TransmissionPass: "new-tpass",
		DBPath:           "/data/bot.db", // preserved from the snapshot
		SubInterval:      30 * time.Minute,
	}
	if *got != *want {
		t.Errorf("saved config mismatch:\n got %+v\nwant %+v", *got, *want)
	}
}

func TestPostBlankSecretsKeepCurrent(t *testing.T) {
	s, store, restarts := newTestServer(testConfig())

	form := validForm()
	form.Set("telegram_token", "")
	form.Set("prowlarr_api_key", "")
	form.Set("transmission_pass", "")
	rec := postForm(t, s, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200", rec.Code)
	}
	if *restarts != 1 || len(store.saved) != 1 {
		t.Fatalf("restarts = %d, saves = %d, want 1 and 1", *restarts, len(store.saved))
	}

	got := store.saved[0]
	if got.TelegramToken != "123456:test-token" {
		t.Errorf("TelegramToken = %q, want current value kept", got.TelegramToken)
	}
	if got.ProwlarrAPIKey != "prowlarr-key" {
		t.Errorf("ProwlarrAPIKey = %q, want current value kept", got.ProwlarrAPIKey)
	}
	if got.TransmissionPass != "tpass" {
		t.Errorf("TransmissionPass = %q, want current value kept", got.TransmissionPass)
	}
}

func TestPostInvalid(t *testing.T) {
	tests := []struct {
		name    string
		field   string
		value   string
		mention string
	}{
		{"non-integer chat id", "allowed_chat_id", "abc", "allowed_chat_id"},
		{"zero chat id", "allowed_chat_id", "0", "allowed_chat_id"},
		{"blank chat id", "allowed_chat_id", "", "allowed_chat_id"},
		{"bad interval", "sub_interval", "soon", "sub_interval"},
		{"negative interval", "sub_interval", "-5m", "sub_interval"},
		{"bad prowlarr url", "prowlarr_url", "not a url", "prowlarr_url"},
		{"schemeless transmission url", "transmission_url", "umbrel.local:9091", "transmission_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, store, restarts := newTestServer(testConfig())

			form := validForm()
			form.Set(tt.field, tt.value)
			rec := postForm(t, s, form)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("POST / status = %d, want 422", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tt.mention) {
				t.Errorf("error page does not mention %s", tt.mention)
			}
			if len(store.saved) != 0 {
				t.Errorf("store.Save called %d times on invalid input, want 0", len(store.saved))
			}
			if *restarts != 0 {
				t.Errorf("restart called %d times on invalid input, want 0", *restarts)
			}
		})
	}
}

func TestPostUnconfiguredRequiresSecrets(t *testing.T) {
	// with no current config there is nothing to keep: blank token is an error
	s, store, restarts := newTestServer(nil)

	form := validForm()
	form.Set("telegram_token", "")
	rec := postForm(t, s, form)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("POST / status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "telegram_token") {
		t.Error("error page does not mention telegram_token")
	}
	if len(store.saved) != 0 || *restarts != 0 {
		t.Errorf("saves = %d, restarts = %d, want 0 and 0", len(store.saved), *restarts)
	}
}

func TestPostInvalidDoesNotEchoSecrets(t *testing.T) {
	s, _, _ := newTestServer(testConfig())

	form := validForm()
	form.Set("allowed_chat_id", "abc") // force a re-render
	rec := postForm(t, s, form)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST / status = %d, want 422", rec.Code)
	}
	body := rec.Body.String()
	for _, secret := range []string{"654321:new-token", "new-prowlarr-key", "new-tpass"} {
		if strings.Contains(body, secret) {
			t.Errorf("re-rendered form echoes submitted secret %q", secret)
		}
	}
	// non-secret input must be kept so the user doesn't retype everything
	if !strings.Contains(body, `value="30m"`) {
		t.Error("re-rendered form dropped the submitted sub interval")
	}
}

func TestPostStoreError(t *testing.T) {
	s, store, restarts := newTestServer(testConfig())
	store.err = errors.New("disk full")

	rec := postForm(t, s, validForm())
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("POST / status = %d, want 500", rec.Code)
	}
	if *restarts != 0 {
		t.Errorf("restart called %d times after failed save, want 0", *restarts)
	}
}

func TestStartShutdown(t *testing.T) {
	s, _, _ := newTestServer(testConfig())
	s.Addr = "127.0.0.1:0"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	time.Sleep(50 * time.Millisecond) // let it bind
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start() returned error on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after context cancellation")
	}
}

func TestStartListenError(t *testing.T) {
	// occupy a port, then point the server at it
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	s, _, _ := newTestServer(testConfig())
	s.Addr = ln.Addr().String()

	if err := s.Start(context.Background()); err == nil {
		t.Error("Start() succeeded on an occupied port, want error")
	}
}
