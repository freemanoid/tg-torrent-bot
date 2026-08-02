// Package web serves the settings page: a plain-HTML form for editing the
// bot configuration from the browser. There is no in-app authentication —
// on Umbrel the app_proxy in front of the bot gates access, as it does for
// every app. Saving writes the config through a ConfigStore and triggers a
// process restart to apply it.
package web

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/config"
)

// DefaultAddr is the listen address; port 8542 matches the Umbrel app
// manifest so the app tile lands on the settings page.
const DefaultAddr = ":8542"

// ConfigStore persists a validated configuration when the settings form is
// submitted. Declared here on the consumer side and faked in tests; main
// backs it with Config.Save to the config file.
type ConfigStore interface {
	Save(cfg *config.Config) error
}

// Server is the settings web server. It holds a snapshot of the effective
// configuration (nil when the bot is unconfigured and running in setup mode)
// and never mutates it: a successful save goes through the store and then
// restarts the process, which reloads everything.
type Server struct {
	// Addr is the listen address for Start, DefaultAddr unless overridden
	// (tests use an ephemeral port).
	Addr string

	cfg     *config.Config // effective config snapshot; nil = setup mode
	store   ConfigStore
	restart func() // triggers the apply-by-restart flow after a save
	log     *slog.Logger
	tmpl    *template.Template
}

// New returns a Server rendering cfg (nil for setup mode), saving through
// store, and calling restart after a successful save.
func New(cfg *config.Config, store ConfigStore, restart func(), log *slog.Logger) *Server {
	return &Server{
		Addr:    DefaultAddr,
		cfg:     cfg,
		store:   store,
		restart: restart,
		log:     log,
		tmpl:    template.Must(template.New("web").Parse(tmplSrc)),
	}
}

// Handler returns the routing handler; exposed so tests can drive the server
// through httptest without a listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /{$}", s.handleForm)
	mux.HandleFunc("POST /{$}", s.handleSave)
	return mux
}

// Start listens on s.Addr and serves until ctx is canceled, then shuts down
// gracefully. Cancellation is the normal way to stop; Start returns nil.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.Addr, err)
	}
	srv := &http.Server{Handler: s.Handler()}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("settings server listening", "addr", ln.Addr().String())

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("web: shutdown: %w", err)
		}
		<-errc // Serve has returned ErrServerClosed
		return nil
	case err := <-errc:
		return fmt.Errorf("web: serve: %w", err)
	}
}

// formView is the template data for the settings form. Secret values are
// never part of it — only Has* flags — so they cannot leak into the HTML.
type formView struct {
	Unconfigured bool
	Error        string

	AllowedChatID    string
	ProwlarrURL      string
	TransmissionURL  string
	TransmissionUser string
	SubInterval      string

	HasTelegramToken    bool
	HasProwlarrAPIKey   bool
	HasTransmissionPass bool
}

func (s *Server) handleForm(w http.ResponseWriter, _ *http.Request) {
	s.render(w, http.StatusOK, "page", s.currentView())
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}
	field := func(name string) string { return strings.TrimSpace(r.PostForm.Get(name)) }

	// re-render view keeps everything the user typed except secrets
	view := formView{
		Unconfigured:        s.cfg == nil,
		AllowedChatID:       field("allowed_chat_id"),
		ProwlarrURL:         field("prowlarr_url"),
		TransmissionURL:     field("transmission_url"),
		TransmissionUser:    field("transmission_user"),
		SubInterval:         field("sub_interval"),
		HasTelegramToken:    s.cfg != nil && s.cfg.TelegramToken != "",
		HasProwlarrAPIKey:   s.cfg != nil && s.cfg.ProwlarrAPIKey != "",
		HasTransmissionPass: s.cfg != nil && s.cfg.TransmissionPass != "",
	}
	fail := func(msg string) {
		view.Error = msg
		s.render(w, http.StatusUnprocessableEntity, "page", view)
	}

	cand := &config.Config{
		TelegramToken:    field("telegram_token"),
		ProwlarrURL:      view.ProwlarrURL,
		ProwlarrAPIKey:   field("prowlarr_api_key"),
		TransmissionURL:  view.TransmissionURL,
		TransmissionUser: view.TransmissionUser,
		TransmissionPass: field("transmission_pass"),
		DBPath:           config.DefaultDBPath,
		SubInterval:      config.DefaultSubInterval,
	}
	if s.cfg != nil {
		cand.DBPath = s.cfg.DBPath
		// blank secret fields keep the current values
		if cand.TelegramToken == "" {
			cand.TelegramToken = s.cfg.TelegramToken
		}
		if cand.ProwlarrAPIKey == "" {
			cand.ProwlarrAPIKey = s.cfg.ProwlarrAPIKey
		}
		if cand.TransmissionPass == "" {
			cand.TransmissionPass = s.cfg.TransmissionPass
		}
	}

	if v := view.AllowedChatID; v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			fail(fmt.Sprintf("allowed_chat_id must be an integer chat ID, got %q", v))
			return
		}
		cand.AllowedChatID = id
	}
	if v := view.SubInterval; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			fail(fmt.Sprintf("sub_interval must be a Go duration (e.g. 20m), got %q", v))
			return
		}
		cand.SubInterval = d
	}
	if err := cand.Validate(); err != nil {
		fail(err.Error())
		return
	}

	if err := s.store.Save(cand); err != nil {
		s.log.Error("save config", "error", err)
		view.Error = "failed to save configuration: " + err.Error()
		s.render(w, http.StatusInternalServerError, "page", view)
		return
	}

	s.log.Info("configuration saved, restarting to apply")
	s.render(w, http.StatusOK, "restarting", nil)
	s.restart()
}

// currentView builds the form view from the config snapshot.
func (s *Server) currentView() formView {
	if s.cfg == nil {
		return formView{
			Unconfigured: true,
			SubInterval:  formatDuration(config.DefaultSubInterval),
		}
	}
	return formView{
		AllowedChatID:       strconv.FormatInt(s.cfg.AllowedChatID, 10),
		ProwlarrURL:         s.cfg.ProwlarrURL,
		TransmissionURL:     s.cfg.TransmissionURL,
		TransmissionUser:    s.cfg.TransmissionUser,
		SubInterval:         formatDuration(s.cfg.SubInterval),
		HasTelegramToken:    s.cfg.TelegramToken != "",
		HasProwlarrAPIKey:   s.cfg.ProwlarrAPIKey != "",
		HasTransmissionPass: s.cfg.TransmissionPass != "",
	}
}

// render executes the named template into a buffer first so a template error
// cannot produce a half-written page.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render template", "template", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w) //nolint:errcheck // nothing to do about a failed client write
}

// formatDuration renders d the way a user would type it: "20m" not "20m0s".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// tmplSrc holds both pages. Plain HTML with minimal inline CSS: no JS, no
// external assets, so it works on a LAN-only box with no internet access.
const tmplSrc = `{{define "style"}}<style>
body{font-family:system-ui,sans-serif;max-width:36rem;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.4rem}
label{display:block;margin-top:1rem;font-weight:600}
input{width:100%;padding:.4rem;margin-top:.25rem;box-sizing:border-box}
.hint{font-size:.8rem;color:#666;margin:.15rem 0 0}
.banner{background:#fff3cd;border:1px solid #ffe69c;padding:.6rem;border-radius:.25rem}
.error{background:#f8d7da;border:1px solid #f1aeb5;padding:.6rem;border-radius:.25rem}
button{margin-top:1.5rem;padding:.5rem 1.5rem;font-size:1rem}
</style>{{end}}
{{define "page"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tg-torrent-bot settings</title>
{{template "style"}}
</head>
<body>
<h1>tg-torrent-bot settings</h1>
{{if .Unconfigured}}<p class="banner">Not configured yet. Fill in the settings below to start the bot.</p>{{end}}
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<form method="post" action="/">
<label for="telegram_token">Telegram bot token</label>
<input type="password" id="telegram_token" name="telegram_token" value="" autocomplete="off">
{{if .HasTelegramToken}}<p class="hint">Leave blank to keep the current value.</p>{{else}}<p class="hint">From @BotFather, e.g. 123456:ABC-DEF…</p>{{end}}
<label for="allowed_chat_id">Allowed chat ID</label>
<input type="text" id="allowed_chat_id" name="allowed_chat_id" value="{{.AllowedChatID}}">
<p class="hint">The single Telegram chat the bot answers to.</p>
<label for="prowlarr_url">Prowlarr URL</label>
<input type="text" id="prowlarr_url" name="prowlarr_url" value="{{.ProwlarrURL}}" placeholder="http://umbrel.local:9696">
<label for="prowlarr_api_key">Prowlarr API key</label>
<input type="password" id="prowlarr_api_key" name="prowlarr_api_key" value="" autocomplete="off">
{{if .HasProwlarrAPIKey}}<p class="hint">Leave blank to keep the current value.</p>{{else}}<p class="hint">Prowlarr → Settings → General → API Key.</p>{{end}}
<label for="transmission_url">Transmission URL</label>
<input type="text" id="transmission_url" name="transmission_url" value="{{.TransmissionURL}}" placeholder="http://umbrel.local:9091">
<label for="transmission_user">Transmission username (optional)</label>
<input type="text" id="transmission_user" name="transmission_user" value="{{.TransmissionUser}}">
<label for="transmission_pass">Transmission password (optional)</label>
<input type="password" id="transmission_pass" name="transmission_pass" value="" autocomplete="off">
{{if .HasTransmissionPass}}<p class="hint">Leave blank to keep the current value.</p>{{end}}
<label for="sub_interval">Subscription check interval</label>
<input type="text" id="sub_interval" name="sub_interval" value="{{.SubInterval}}">
<p class="hint">Go duration, e.g. 20m or 1h30m.</p>
<button type="submit">Save and restart</button>
</form>
</body>
</html>{{end}}
{{define "restarting"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tg-torrent-bot settings</title>
<meta http-equiv="refresh" content="5;url=/">
{{template "style"}}
</head>
<body>
<h1>Settings saved</h1>
<p>Restarting to apply the new settings&hellip; this page reloads in a few seconds.</p>
</body>
</html>{{end}}
`
