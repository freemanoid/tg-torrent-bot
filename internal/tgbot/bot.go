// Package tgbot implements the Telegram side of the bot: the chat allowlist,
// the plain-text search flow with inline-keyboard results, and the callback
// handlers that hand releases to Transmission.
package tgbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Bot wires Handlers into a go-telegram/bot long-polling client.
type Bot struct {
	api     *bot.Bot
	chatIDs []int64
	log     *slog.Logger
}

// New builds the Telegram bot: every update must come from one of the allowed
// chats, callback queries go to the keyboard handler, and any unmatched text
// message is treated as a search query. Extra options (e.g. bot.WithSkipGetMe
// in tests) are applied after the defaults.
func New(token string, allowedChatIDs []int64, h *Handlers, extra ...bot.Option) (*Bot, error) {
	opts := append([]bot.Option{
		bot.WithMiddlewares(allowChats(allowedChatIDs)),
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
	return &Bot{api: api, chatIDs: slices.Clone(allowedChatIDs), log: h.log}, nil
}

// Notify sends a plain-text message to every allowed chat. It makes *Bot the
// production Notifier for the subscription engine and completion watcher.
// Subscriptions and downloads belong to the install rather than to whoever
// created them, so everyone on the allowlist hears about them.
//
// A chat that cannot be reached — blocked bot, deleted group — costs only its
// own message: the others are still sent, and an error comes back only when
// none got through, so one broken chat cannot silence the rest.
func (b *Bot) Notify(ctx context.Context, text string) error {
	return b.notify(ctx, text, nil)
}

// NotifyGrab is Notify for a torrent the bot decided to download on its own:
// the same message, plus a button that rejects it.
//
// Every allowed chat gets its own copy of that button, all pointing at the one
// torrent. Whoever taps first does the removal and the rest go stale, which is
// exactly why the removal treats an already-gone torrent as success.
func (b *Bot) NotifyGrab(ctx context.Context, text, hash string) error {
	return b.notify(ctx, text, rejectKeyboard(hash))
}

func (b *Bot) notify(ctx context.Context, text string, kb models.ReplyMarkup) error {
	var errs []error
	for _, id := range b.chatIDs {
		_, err := b.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: id, Text: text, ReplyMarkup: kb})
		if err != nil {
			b.log.Warn("telegram notify", "chat_id", id, "error", err)
			errs = append(errs, err)
		}
	}
	if len(errs) == len(b.chatIDs) && len(errs) > 0 {
		return fmt.Errorf("telegram notify: %w", errors.Join(errs...))
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

// allowChats drops every update that does not originate from one of the
// allowed chats. Unauthorized senders get no reply at all — the bot stays
// invisible to them.
func allowChats(allowed []int64) bot.Middleware {
	allowed = slices.Clone(allowed)
	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			if update == nil || !slices.Contains(allowed, updateChatID(update)) {
				return
			}
			next(ctx, b, update)
		}
	}
}

// updateChatID extracts the originating chat ID from the update kinds the bot
// consumes — plain messages and callback queries — or 0 for anything else
// (config rejects 0 in the allowlist, so such updates are always dropped).
// Edited messages deliberately fall through to 0: no handler consumes them.
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
