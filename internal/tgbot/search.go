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
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/torrentmeta"
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
		h.answer(ctx, api, ack, "Search failed: "+err.Error(), nil)
		return
	}
	if len(releases) == 0 {
		h.answer(ctx, api, ack, fmt.Sprintf("No results for «%s».", query), nil)
		return
	}

	id := h.cache.Put(cachedSearch{Query: query, Releases: releases})
	marks := h.pageMarks(ctx, releases, 0)
	h.answer(ctx, api, ack,
		resultsMessage(query, releases, 0, marks),
		resultsKeyboard(id, releases, 0, marks))
}

// pageMarks reports which releases on the given page the bot already grabbed,
// as a marker per release index.
//
// It is computed at render time rather than cached with the search: a page flip
// re-renders the whole message, and a torrent that finishes while the message
// is open should show its new state on the next tap. Only the page's releases
// are looked up, so the query stays bounded however large the table grows.
//
// A store failure yields no marks, never an error: the markers are a
// convenience, and losing them must not cost a search that took minutes to run.
func (h *Handlers) pageMarks(ctx context.Context, releases []prowlarr.Release, page int) map[int]string {
	start, end := pageBounds(len(releases), page)
	if start >= end {
		return nil
	}

	hashes := make([]string, 0, end-start)
	titles := make([]string, 0, end-start)
	for _, r := range releases[start:end] {
		if r.InfoHash != "" {
			hashes = append(hashes, r.InfoHash)
		}
		titles = append(titles, r.Title)
	}

	found, err := h.subs.FindDownloads(ctx, hashes, titles)
	if err != nil {
		h.log.Warn("look up download state", "error", err)
		return nil
	}

	byHash := make(map[string]store.Download, len(found))
	byTitle := make(map[string]store.Download, len(found))
	for _, dl := range found {
		byHash[strings.ToLower(dl.Hash)] = dl
		byTitle[dl.Title] = dl
	}

	marks := make(map[int]string, end-start)
	for i := start; i < end; i++ {
		r := releases[i]
		// The hash identifies the release even when an indexer words its title
		// differently, so it wins; the title covers the releases that carry no
		// hash at all.
		dl, ok := byHash[strings.ToLower(r.InfoHash)]
		if !ok || r.InfoHash == "" {
			dl, ok = byTitle[r.Title]
		}
		if !ok {
			continue
		}
		if m := statusMark(dl.Status); m != "" {
			marks[i] = m
		}
	}
	return marks
}

// ackSearching posts a placeholder before a Prowlarr search so the chat shows
// the query was picked up: a search regularly runs for minutes (a cold
// FlareSolverr Cloudflare challenge alone measured ~193 s) and a silent bot
// looks stuck.
func (h *Handlers) ackSearching(ctx context.Context, api telegramAPI, query string) int {
	return h.ack(ctx, api, fmt.Sprintf("🔎 Searching «%s»…", query))
}

// ack posts a placeholder message and returns the ID to edit the real answer
// into, or 0 when it could not be sent — losing the ack must never cost the
// user their answer.
func (h *Handlers) ack(ctx context.Context, api telegramAPI, text string) int {
	msg, err := api.SendMessage(ctx, &bot.SendMessageParams{ChatID: h.chatID, Text: text})
	if err != nil {
		h.log.Warn("send ack", "error", err)
		return 0
	}
	return msg.ID
}

// answer turns an ack message into the final answer, falling back to a fresh
// message when there is no ack or the edit fails. An answer that carries a
// keyboard must stay one message — the keyboard is attached to it — so it is
// never chunked; resultsMessage and detailsMessage are what keep those inside
// Telegram's limit. The nil-keyboard path still routes through h.reply for the
// odd long error.
func (h *Handlers) answer(ctx context.Context, api telegramAPI, ack int, text string, kb models.ReplyMarkup) {
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
		h.log.Warn("edit ack", "error", err)
	}
	if kb == nil {
		h.reply(ctx, api, text)
		return
	}
	if _, err := api.SendMessage(ctx, &bot.SendMessageParams{ChatID: h.chatID, Text: text, ReplyMarkup: kb}); err != nil {
		h.log.Error("send answer", "error", err)
	}
}

// HandleCallback processes inline-keyboard taps: dl:<id>:<idx> downloads a
// release, if:<id>:<idx> describes one in full, pg:<id>:<page> flips the
// keyboard to another page.
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
	case cbInfo:
		h.details(ctx, api, searchID, entry, n)
	default:
		h.log.Warn("unknown callback kind", "data", cb.Data)
	}
}

// flipPage swaps the result message's keyboard (and header) to another page.
func (h *Handlers) flipPage(ctx context.Context, api telegramAPI, cb *models.CallbackQuery, searchID string, entry cachedSearch, page int) {
	page = clampPage(len(entry.Releases), page)
	marks := h.pageMarks(ctx, entry.Releases, page)
	text := resultsMessage(entry.Query, entry.Releases, page, marks)
	kb := resultsKeyboard(searchID, entry.Releases, page, marks)

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

// details answers a ℹ️ tap with everything the bot can learn about one release
// without downloading it: its untruncated title and detail lines, what the
// indexer says about it, and the file list read out of the .torrent. It is
// sent as its own message carrying a download button, so the results message —
// and the search behind it — stays intact underneath.
func (h *Handlers) details(ctx context.Context, api telegramAPI, searchID string, entry cachedSearch, idx int) {
	if idx < 0 || idx >= len(entry.Releases) {
		h.reply(ctx, api, "That result is no longer available — please search again.")
		return
	}
	r := entry.Releases[idx]

	// Fetching the .torrent goes through Prowlarr and the tracker, so it is
	// slow often enough to deserve the same ack a search gets.
	ack := h.ack(ctx, api, fmt.Sprintf("📄 Reading «%s»…", truncate(r.Title, maxHeaderQueryRunes)))

	meta, unavailable := h.releaseMeta(ctx, r)
	marks := h.pageMarks(ctx, entry.Releases, idx/perPage)
	h.answer(ctx, api, ack,
		detailsMessage(idx+1, r, marks[idx], meta, unavailable),
		detailsKeyboard(searchID, idx))
}

// releaseMeta fetches and parses the release's .torrent for its file list,
// returning nil plus a reason the details view can show when that is not
// possible. Failing to read it is ordinary, not exceptional: a magnet-only
// release has no .torrent to fetch, and Prowlarr answers magnet-backed
// downloadUrls with a redirect to a magnet: URI the HTTP fetch cannot follow.
//
// The bytes are deliberately not cached. A .torrent can be tens of megabytes
// and the host is a RAM-tight Pi shared with every other Umbrel app; holding
// blobs for an hour to save a rare second tap is the wrong trade.
func (h *Handlers) releaseMeta(ctx context.Context, r prowlarr.Release) (*torrentmeta.Meta, string) {
	if r.DownloadURL == "" {
		return nil, "file list unavailable: magnet-only release."
	}
	raw, err := h.searcher.FetchTorrent(ctx, r.DownloadURL)
	if err != nil {
		h.log.Warn("fetch torrent for details", "title", r.Title, "error", err)
		return nil, "file list unavailable: the .torrent could not be fetched."
	}
	meta, err := torrentmeta.Parse(raw)
	if err != nil {
		h.log.Warn("parse torrent for details", "title", r.Title, "error", err)
		return nil, "file list unavailable: the .torrent could not be read."
	}
	return &meta, ""
}

// detailsKeyboard puts the download one tap away from the details that
// justified it, reusing the results message's own download callback.
func detailsKeyboard(searchID string, idx int) *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{{
		Text:         "⬇️ Download",
		CallbackData: encodeCallback(cbDownload, searchID, idx),
	}}}}
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

// resultsKeyboard builds one page of result buttons plus a pager row. marks
// carries the download-state marker per release index and may be nil.
func resultsKeyboard(searchID string, releases []prowlarr.Release, page int, marks map[int]string) *models.InlineKeyboardMarkup {
	page = clampPage(len(releases), page)
	start, end := pageBounds(len(releases), page)

	rows := make([][]models.InlineKeyboardButton, 0, end-start+3)
	for i := start; i < end; i++ {
		rows = append(rows, []models.InlineKeyboardButton{{
			Text:         buttonLabel(i+1, releases[i], marks[i]),
			CallbackData: encodeCallback(cbDownload, searchID, i),
		}})
	}

	// The details buttons share one row rather than sitting next to their
	// result: Telegram splits a row's width evenly, and pairing them would
	// halve the quality summary the result label is there to carry.
	info := make([]models.InlineKeyboardButton, 0, end-start)
	for i := start; i < end; i++ {
		info = append(info, models.InlineKeyboardButton{
			Text:         fmt.Sprintf("ℹ️%d", i+1),
			CallbackData: encodeCallback(cbInfo, searchID, i),
		})
	}
	if len(info) > 0 {
		rows = append(rows, info)
	}

	// Tracker pages ride in a row of their own, for the same reason the details
	// buttons do. They are link buttons rather than callbacks — Telegram opens
	// the page itself — and cost the message text nothing, which matters
	// because the results message shares a tight rune budget between five
	// blocks. A release whose indexer published no usable page simply has no
	// button; the numbers say which result each link belongs to.
	links := make([]models.InlineKeyboardButton, 0, end-start)
	for i := start; i < end; i++ {
		if u := linkURL(releases[i]); u != "" {
			links = append(links, models.InlineKeyboardButton{
				Text: fmt.Sprintf("🔗%d", i+1),
				URL:  u,
			})
		}
	}
	if len(links) > 0 {
		rows = append(rows, links)
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
