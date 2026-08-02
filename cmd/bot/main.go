// Command bot is the tg-torrent-bot entry point: it wires together the
// Telegram bot, subscription engine, completion watcher, and settings web
// server, and runs them until SIGINT/SIGTERM. When no complete configuration
// is found it starts in setup mode — the settings page alone — so a fresh
// install can be configured from the browser instead of over SSH.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"golang.org/x/sync/errgroup"

	"github.com/freemanoid/tg-torrent-bot/internal/config"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/subs"
	"github.com/freemanoid/tg-torrent-bot/internal/tgbot"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
	"github.com/freemanoid/tg-torrent-bot/internal/web"
)

const (
	// defaultConfigPath is where the settings page persists configuration:
	// the /data volume, next to the SQLite database, is the only writable
	// location that survives an image update. Override with CONFIG_PATH.
	defaultConfigPath = "/data/config.json"

	// restartDelay gives the "restarting…" response time to reach the
	// browser before the process exits to apply new settings.
	restartDelay = 250 * time.Millisecond
)

// app owns every long-lived component and runs them as one unit. In setup
// mode only web is wired: with no usable configuration there is no database
// to open and nothing to talk to Telegram with, so the other fields are nil.
type app struct {
	st       *store.Store
	prowlarr *prowlarr.Client
	trans    *transmission.Client
	bot      *tgbot.Bot
	engine   *subs.Engine
	watcher  *subs.Watcher
	web      *web.Server
	log      *slog.Logger
}

// appOpts carries what newApp needs besides the configuration itself. store
// and restart back the settings page and are both required; botOpts is extra
// Telegram client options (e.g. bot.WithSkipGetMe in tests).
type appOpts struct {
	store   web.ConfigStore // persists a settings-form submission
	restart func()          // applies a saved configuration by restarting
	log     *slog.Logger
	botOpts []bot.Option
}

// newApp wires real components from cfg: SQLite store (opened and migrated),
// Prowlarr and Transmission clients, the Telegram bot, the subscription
// engine, the completion watcher, and the settings server. A nil cfg means
// the bot is not configured yet and wires the settings server alone.
func newApp(cfg *config.Config, o appOpts) (*app, error) {
	ws := web.New(cfg, o.store, o.restart, o.log)
	if cfg == nil {
		return &app{web: ws, log: o.log}, nil
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	pr, err := prowlarr.New(cfg.ProwlarrURL, cfg.ProwlarrAPIKey)
	if err != nil {
		st.Close()
		return nil, err
	}

	tr, err := transmission.New(cfg.TransmissionURL, cfg.TransmissionUser, cfg.TransmissionPass)
	if err != nil {
		st.Close()
		return nil, err
	}

	handlers := tgbot.NewHandlers(cfg.AllowedChatID, pr, tr, st, o.log)
	tb, err := tgbot.New(cfg.TelegramToken, cfg.AllowedChatID, handlers, o.botOpts...)
	if err != nil {
		st.Close()
		return nil, err
	}

	return &app{
		st:       st,
		prowlarr: pr,
		trans:    tr,
		bot:      tb,
		engine:   subs.NewEngine(st, pr, tr, tb, cfg.SubInterval, o.log),
		watcher:  subs.NewWatcher(st, tr, tb, subs.DefaultWatchInterval, o.log),
		web:      ws,
		log:      o.log,
	}, nil
}

// setupMode reports whether the app came up without a usable configuration
// and therefore runs the settings page on its own.
func (a *app) setupMode() bool { return a.st == nil }

// Run performs the startup self-check, then runs the long-lived loops —
// Telegram long polling, subscription ticker, completion watcher, settings
// server — until ctx is canceled. Cancellation is the normal way to stop;
// Run returns nil. In setup mode the settings page is the whole app, so it
// runs alone and its failure is the app's failure.
func (a *app) Run(ctx context.Context) error {
	if a.setupMode() {
		a.log.Info("setup mode: open the app and fill in the settings page to start the bot")
		return a.web.Start(ctx)
	}

	a.selfCheck(ctx)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { a.bot.Run(ctx); return nil })
	g.Go(func() error { return a.engine.Run(ctx) })
	g.Go(func() error { return a.watcher.Run(ctx) })
	g.Go(func() error {
		// Like every other loop, a settings server that dies is logged, not
		// fatal: the bot's actual job is downloading, and it keeps working
		// with the configuration it already has.
		if err := a.web.Start(ctx); err != nil {
			a.log.Error("settings server stopped", "error", err)
		}
		return nil
	})
	return g.Wait()
}

// selfCheck pings Prowlarr and Transmission at startup. Failures are logged
// as warnings, never fatal: on an Umbrel box the bot may simply come up
// before its neighbors, and every loop retries on its own schedule anyway.
func (a *app) selfCheck(ctx context.Context) {
	if err := a.prowlarr.Ping(ctx); err != nil {
		a.log.Warn("prowlarr unreachable at startup", "error", err)
	} else {
		a.log.Info("prowlarr reachable")
	}
	if _, err := a.trans.Active(ctx); err != nil {
		a.log.Warn("transmission unreachable at startup", "error", err)
	} else {
		a.log.Info("transmission reachable")
	}
}

// Close releases resources that outlive Run's context (the SQLite store).
// In setup mode there is nothing to release.
func (a *app) Close() error {
	if a.setupMode() {
		return nil
	}
	return a.st.Close()
}

// configFile persists settings-form submissions to the JSON config file,
// adapting config.Config.Save to the web.ConfigStore interface.
type configFile string

func (p configFile) Save(cfg *config.Config) error { return cfg.Save(string(p)) }

// configPath returns the settings file location: CONFIG_PATH when set,
// /data/config.json otherwise.
func configPath() string {
	if p := os.Getenv("CONFIG_PATH"); p != "" {
		return p
	}
	return defaultConfigPath
}

// restarter returns the hook the settings page calls after a successful
// save. A single-binary process applies new configuration the simplest way
// available: exit non-zero and let the container restart policy start it
// again, reading the file that was just written. The exit runs in its own
// goroutine after delay so the HTTP handler can return and flush the
// "restarting…" response first. exit is injected so tests can observe the
// hook without dying.
func restarter(log *slog.Logger, delay time.Duration, exit func(int)) func() {
	return func() {
		go func() {
			time.Sleep(delay)
			log.Info("restarting to apply the new configuration")
			exit(1)
		}()
	}
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("failed", "error", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

// run loads the config, wires the app, and runs it until SIGINT/SIGTERM. It
// is the single exit path for the normal lifecycle, so os.Exit never
// bypasses the deferred store Close. The one deliberate exception is the
// settings-page restart hook, which exits on purpose to reload the process;
// that is safe because SQLite replays its write-ahead log on the next open.
func run(log *slog.Logger) error {
	path := configPath()

	cfg, err := config.LoadFrom(path)
	var incomplete *config.ErrIncomplete
	switch {
	case errors.As(err, &incomplete):
		// Not a failure: a fresh install has nothing configured yet. Run
		// the settings page alone so the browser can finish the setup.
		log.Warn("configuration incomplete, starting the settings page only",
			"missing", strings.Join(incomplete.Missing, ", "), "config_file", path)
		cfg = nil
	case err != nil:
		return fmt.Errorf("load config: %w", err)
	}

	a, err := newApp(cfg, appOpts{
		store:   configFile(path),
		restart: restarter(log, restartDelay, os.Exit),
		log:     log,
	})
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg != nil {
		log.Info("starting",
			"prowlarr", cfg.ProwlarrURL,
			"transmission", cfg.TransmissionURL,
			"db", cfg.DBPath,
			"config_file", path,
			"sub_interval", cfg.SubInterval,
		)
	}
	return a.Run(ctx)
}
