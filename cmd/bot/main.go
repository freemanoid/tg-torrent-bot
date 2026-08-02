// Command bot is the tg-torrent-bot entry point: it wires together the
// Telegram bot, subscription engine, and completion watcher, and runs them
// until SIGINT/SIGTERM.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-telegram/bot"
	"golang.org/x/sync/errgroup"

	"github.com/freemanoid/tg-torrent-bot/internal/config"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/subs"
	"github.com/freemanoid/tg-torrent-bot/internal/tgbot"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// app owns every long-lived component and runs them as one unit.
type app struct {
	st       *store.Store
	prowlarr *prowlarr.Client
	trans    *transmission.Client
	bot      *tgbot.Bot
	engine   *subs.Engine
	watcher  *subs.Watcher
	log      *slog.Logger
}

// newApp wires real components from cfg: SQLite store (opened and migrated),
// Prowlarr and Transmission clients, the Telegram bot, the subscription
// engine, and the completion watcher. Extra bot options (e.g.
// bot.WithSkipGetMe in tests) are forwarded to the Telegram client.
func newApp(cfg *config.Config, log *slog.Logger, botOpts ...bot.Option) (*app, error) {
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

	handlers := tgbot.NewHandlers(cfg.AllowedChatID, pr, tr, st, log)
	tb, err := tgbot.New(cfg.TelegramToken, cfg.AllowedChatID, handlers, botOpts...)
	if err != nil {
		st.Close()
		return nil, err
	}

	return &app{
		st:       st,
		prowlarr: pr,
		trans:    tr,
		bot:      tb,
		engine:   subs.NewEngine(st, pr, tr, tb, cfg.SubInterval, log),
		watcher:  subs.NewWatcher(st, tr, tb, subs.DefaultWatchInterval, log),
		log:      log,
	}, nil
}

// Run performs the startup self-check, then runs the three long-lived loops
// — Telegram long polling, subscription ticker, completion watcher — until
// ctx is canceled. Cancellation is the normal way to stop; Run returns nil.
func (a *app) Run(ctx context.Context) error {
	a.selfCheck(ctx)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { a.bot.Run(ctx); return nil })
	g.Go(func() error { return a.engine.Run(ctx) })
	g.Go(func() error { return a.watcher.Run(ctx) })
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
func (a *app) Close() error {
	return a.st.Close()
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
// is the single exit path out of main, so os.Exit never bypasses the deferred
// store Close.
func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	a, err := newApp(cfg, log)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting",
		"prowlarr", cfg.ProwlarrURL,
		"transmission", cfg.TransmissionURL,
		"db", cfg.DBPath,
		"sub_interval", cfg.SubInterval,
	)
	return a.Run(ctx)
}
