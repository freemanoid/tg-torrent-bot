// Package tgbot implements the Telegram side of the bot: the single-chat
// allowlist, the plain-text search flow with inline-keyboard results, and the
// callback handlers that hand releases to Transmission.
package tgbot

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Bot wires Handlers into a go-telegram/bot long-polling client.
type Bot struct {
	api    *bot.Bot
	chatID int64
	log    *slog.Logger
}

// New builds the Telegram bot: every update must pass the allowed-chat check,
// callback queries go to the keyboard handler, and any unmatched text message
// is treated as a search query. Extra options (e.g. bot.WithSkipGetMe in
// tests) are applied after the defaults.
func New(token string, allowedChatID int64, h *Handlers, extra ...bot.Option) (*Bot, error) {
	opts := append([]bot.Option{
		bot.WithMiddlewares(allowChat(allowedChatID)),
		bot.WithCallbackQueryDataHandler("", bot.MatchTypePrefix, func(ctx context.Context, b *bot.Bot, update *models.Update) {
			h.HandleCallback(ctx, b, update)
		}),
		bot.WithDefaultHandler(func(ctx context.Context, b *bot.Bot, update *models.Update) {
			h.HandleText(ctx, b, update)
		}),
	}, extra...)

	api, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &Bot{api: api, chatID: allowedChatID, log: h.log}, nil
}

// Notify sends a plain-text message to the allowed chat. It makes *Bot the
// production Notifier for the subscription engine and completion watcher.
func (b *Bot) Notify(ctx context.Context, text string) error {
	if _, err := b.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: b.chatID, Text: text}); err != nil {
		return fmt.Errorf("telegram notify: %w", err)
	}
	return nil
}

// Run registers the command menu, then starts long polling and blocks until
// ctx is cancelled. A failed menu registration is only a cosmetic loss, so it
// is logged rather than fatal.
func (b *Bot) Run(ctx context.Context) {
	if _, err := b.api.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: commandMenu()}); err != nil {
		b.log.Warn("register telegram command menu", "error", err)
	}
	b.api.Start(ctx)
}

// allowChat drops every update that does not originate from the allowed chat.
// Unauthorized senders get no reply at all — the bot stays invisible to them.
func allowChat(allowed int64) bot.Middleware {
	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			if update == nil || updateChatID(update) != allowed {
				return
			}
			next(ctx, b, update)
		}
	}
}

// updateChatID extracts the originating chat ID from the update kinds the bot
// consumes — plain messages and callback queries — or 0 for anything else
// (config.Load rejects ALLOWED_CHAT_ID=0, so such updates are always
// dropped). Edited messages deliberately fall through to 0: no handler
// consumes them.
func updateChatID(u *models.Update) int64 {
	switch {
	case u.Message != nil:
		return u.Message.Chat.ID
	case u.CallbackQuery != nil:
		if m := u.CallbackQuery.Message.Message; m != nil {
			return m.Chat.ID
		}
		if m := u.CallbackQuery.Message.InaccessibleMessage; m != nil {
			return m.Chat.ID
		}
	}
	return 0
}
