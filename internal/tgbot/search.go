package tgbot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/freemanoid/tg-torrent-bot/internal/grab"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// sourceSearch tags downloads started from an interactive search.
const sourceSearch = "search"

// maxMessageLen is Telegram's per-message text limit (in characters); longer
// replies are split into several messages at line boundaries.
const maxMessageLen = 4096

// Searcher is the Prowlarr surface the handlers use; *prowlarr.Client
// implements it, tests fake it.
type Searcher interface {
	Search(ctx context.Context, query string) ([]prowlarr.Release, error)
	FetchTorrent(ctx context.Context, downloadURL string) ([]byte, error)
}

// telegramAPI is the slice of *bot.Bot the handlers call; faked in tests.
type telegramAPI interface {
	SendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error)
	EditMessageText(ctx context.Context, params *bot.EditMessageTextParams) (*models.Message, error)
	AnswerCallbackQuery(ctx context.Context, params *bot.AnswerCallbackQueryParams) (bool, error)
}

// Handlers implements the Telegram update handlers for the interactive search
// flow: plain text searches, result keyboards, pagination, and downloads.
type Handlers struct {
	chatID   int64
	searcher Searcher
	trans    transmission.Interface
	subs     SubscriptionStore
	cache    *searchCache
	log      *slog.Logger
}

// NewHandlers wires the search-flow handlers. chatID is the single allowed
// chat; every reply goes there. A nil log falls back to slog.Default().
func NewHandlers(chatID int64, searcher Searcher, trans transmission.Interface, subs SubscriptionStore, log *slog.Logger) *Handlers {
	if log == nil {
		log = slog.Default()
	}
	return &Handlers{
		chatID:   chatID,
		searcher: searcher,
		trans:    trans,
		subs:     subs,
		cache:    newSearchCache(cacheTTL),
		log:      log,
	}
}

// HandleText routes text messages: /commands go to the command handlers, any
// other text is a search query answered with an inline keyboard of results
// sorted by seeders.
func (h *Handlers) HandleText(ctx context.Context, api telegramAPI, update *models.Update) {
	if update.Message == nil {
		return
	}
	query := strings.TrimSpace(update.Message.Text)
	if query == "" {
		return
	}
	if strings.HasPrefix(query, "/") {
		h.handleCommand(ctx, api, query)
		return
	}

	ack := h.ackSearching(ctx, api, query)

	releases, err := h.searcher.Search(ctx, query)
	if err != nil {
		h.answerSearch(ctx, api, ack, "Search failed: "+err.Error(), nil)
		return
	}
	if len(releases) == 0 {
		h.answerSearch(ctx, api, ack, fmt.Sprintf("No results for «%s».", query), nil)
		return
	}

	id := h.cache.Put(cachedSearch{Query: query, Releases: releases})
	h.answerSearch(ctx, api, ack,
		resultsHeader(query, len(releases), 0),
		resultsKeyboard(id, releases, 0))
}

// ackSearching posts a placeholder before a Prowlarr search so the chat shows
// the query was picked up: a search regularly runs for minutes (a cold
// FlareSolverr Cloudflare challenge alone measured ~193 s) and a silent bot
// looks stuck. It returns the message ID to edit the answer into, or 0 when
// the ack could not be sent — losing it must never cost the user their answer.
func (h *Handlers) ackSearching(ctx context.Context, api telegramAPI, query string) int {
	msg, err := api.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: h.chatID,
		Text:   fmt.Sprintf("🔎 Searching «%s»…", query),
	})
	if err != nil {
		h.log.Warn("send search ack", "error", err)
		return 0
	}
	return msg.ID
}

// answerSearch turns the ack message into the final answer, falling back to a
// fresh message when there is no ack or the edit fails. Every answer it is
// given is a single short message, so unlike h.reply it needs no chunking —
// the nil-keyboard path still routes through h.reply for the odd long error.
func (h *Handlers) answerSearch(ctx context.Context, api telegramAPI, ack int, text string, kb models.ReplyMarkup) {
	if ack != 0 {
		_, err := api.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      h.chatID,
			MessageID:   ack,
			Text:        text,
			ReplyMarkup: kb,
		})
		if err == nil {
			return
		}
		h.log.Warn("edit search ack", "error", err)
	}
	if kb == nil {
		h.reply(ctx, api, text)
		return
	}
	if _, err := api.SendMessage(ctx, &bot.SendMessageParams{ChatID: h.chatID, Text: text, ReplyMarkup: kb}); err != nil {
		h.log.Error("send search results", "error", err)
	}
}

// HandleCallback processes inline-keyboard taps: dl:<id>:<idx> downloads a
// release, pg:<id>:<page> flips the keyboard to another page.
func (h *Handlers) HandleCallback(ctx context.Context, api telegramAPI, update *models.Update) {
	cb := update.CallbackQuery
	if cb == nil {
		return
	}
	// Stop the button spinner right away; fetching a torrent can take a while.
	if _, err := api.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID}); err != nil {
		h.log.Warn("answer callback query", "error", err)
	}

	kind, searchID, n, err := decodeCallback(cb.Data)
	if err != nil {
		h.log.Warn("bad callback data", "data", cb.Data)
		return
	}
	entry, ok := h.cache.Get(searchID)
	if !ok {
		h.reply(ctx, api, "That search has expired — please send the query again.")
		return
	}

	switch kind {
	case cbPage:
		h.flipPage(ctx, api, cb, searchID, entry, n)
	case cbDownload:
		h.download(ctx, api, entry, n)
	default:
		h.log.Warn("unknown callback kind", "data", cb.Data)
	}
}

// flipPage swaps the result message's keyboard (and header) to another page.
func (h *Handlers) flipPage(ctx context.Context, api telegramAPI, cb *models.CallbackQuery, searchID string, entry cachedSearch, page int) {
	page = clampPage(len(entry.Releases), page)
	text := resultsHeader(entry.Query, len(entry.Releases), page)
	kb := resultsKeyboard(searchID, entry.Releases, page)

	if msg := cb.Message.Message; msg != nil {
		_, err := api.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:      h.chatID,
			MessageID:   msg.ID,
			Text:        text,
			ReplyMarkup: kb,
		})
		if err == nil {
			return
		}
		h.log.Warn("edit results page", "error", err)
	}
	// Original message unavailable (or edit failed): send the page fresh.
	if _, err := api.SendMessage(ctx, &bot.SendMessageParams{ChatID: h.chatID, Text: text, ReplyMarkup: kb}); err != nil {
		h.log.Error("send results page", "error", err)
	}
}

// download hands the tapped release to Transmission and confirms in chat.
func (h *Handlers) download(ctx context.Context, api telegramAPI, entry cachedSearch, idx int) {
	if idx < 0 || idx >= len(entry.Releases) {
		h.reply(ctx, api, "That result is no longer available — please search again.")
		return
	}
	r := entry.Releases[idx]

	hash, err := grab.AddRelease(ctx, h.searcher, h.trans, r)
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Failed to add «%s»: %v", r.Title, err))
		return
	}
	if err := h.subs.AddDownload(ctx, hash, r.Title, sourceSearch); err != nil {
		// The torrent is already downloading; only completion notification is lost.
		h.log.Error("record download", "hash", hash, "error", err)
	}
	h.reply(ctx, api, "⬇️ Added: "+r.Title)
}

// reply sends a plain-text message to the allowed chat, splitting texts past
// Telegram's length limit into several messages so long lists (/subs,
// /status) degrade gracefully instead of failing to send at all.
func (h *Handlers) reply(ctx context.Context, api telegramAPI, text string) {
	for _, chunk := range splitMessage(text, maxMessageLen) {
		if _, err := api.SendMessage(ctx, &bot.SendMessageParams{ChatID: h.chatID, Text: chunk}); err != nil {
			h.log.Error("send message", "error", err)
			return
		}
	}
}

// splitMessage splits text into chunks of at most limit runes, cutting at the
// last newline within the limit when there is one so entries stay whole.
func splitMessage(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(runes) > limit {
		cut := limit
		for i := limit; i > 0; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		if chunk := strings.Trim(string(runes[:cut]), "\n"); chunk != "" {
			chunks = append(chunks, chunk)
		}
		runes = runes[cut:]
	}
	if chunk := strings.Trim(string(runes), "\n"); chunk != "" {
		chunks = append(chunks, chunk)
	}
	return chunks
}

// resultsKeyboard builds one page of result buttons plus a pager row.
func resultsKeyboard(searchID string, releases []prowlarr.Release, page int) *models.InlineKeyboardMarkup {
	page = clampPage(len(releases), page)
	start, end := pageBounds(len(releases), page)

	rows := make([][]models.InlineKeyboardButton, 0, end-start+1)
	for i := start; i < end; i++ {
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         buttonLabel(i+1, releases[i]),
			CallbackData: encodeCallback(cbDownload, searchID, i),
		}})
	}

	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, models.InlineKeyboardButton{
			Text:         "⏮ Prev",
			CallbackData: encodeCallback(cbPage, searchID, page-1),
		})
	}
	if page < numPages(len(releases))-1 {
		nav = append(nav, models.InlineKeyboardButton{
			Text:         "⏭ Next",
			CallbackData: encodeCallback(cbPage, searchID, page+1),
		})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}
