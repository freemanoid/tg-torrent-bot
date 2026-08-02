package tgbot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-telegram/bot/models"

	"github.com/freemanoid/tg-torrent-bot/internal/filter"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// SubscriptionStore is the store surface the handlers use; *store.Store
// implements it, tests fake it. It deliberately excludes the seen-table
// methods: commands (including the /test dry run) can never touch seen state.
type SubscriptionStore interface {
	CreateSubscription(ctx context.Context, sub store.Subscription) (store.Subscription, error)
	GetSubscription(ctx context.Context, id int64) (store.Subscription, error)
	ListSubscriptions(ctx context.Context) ([]store.Subscription, error)
	DeleteSubscription(ctx context.Context, id int64) error
	SetSubscriptionPaused(ctx context.Context, id int64, paused bool) error
	ActiveDownloads(ctx context.Context) ([]store.Download, error)
	// AddDownload records a torrent handed to Transmission so the completion
	// watcher can notify when it finishes.
	AddDownload(ctx context.Context, hash, title, source string) error
}

var _ SubscriptionStore = (*store.Store)(nil)

// maxDryRunLines caps how many matching releases /test lists in one message.
const maxDryRunLines = 10

// lastCheckedFormat renders subscription check timestamps in /subs.
const lastCheckedFormat = "2006-01-02 15:04"

const subUsage = "Usage: /sub <query> | <filters>\n" +
	"Example: /sub space show 2026 | rus, 1080p, x265, -720p, >1gb"

const helpText = `🔍 Send any text to search for torrents; tap a result to download it.

Commands:
/sub <query> | <filters> — subscribe to a recurring search
/subs — list subscriptions
/unsub <id> — remove a subscription
/pause <id> — pause or resume a subscription
/test <id> — dry run: show what would match now, download nothing
/status — active downloads with progress
/help — this message

Filters (comma-separated, case-insensitive, all optional):
  rus — required substring
  -720p — excluded substring
  /x26[45]/ — required regex, -/cam|ts/ — excluded regex
  >1gb / <30gb — size bounds (mb or gb)`

// commandMenu is the command list registered with Telegram so the client
// offers a menu button; Telegram wants commands without the leading slash.
func commandMenu() []models.BotCommand {
	return []models.BotCommand{
		{Command: "sub", Description: "Subscribe: /sub <query> | <filters>"},
		{Command: "subs", Description: "List subscriptions"},
		{Command: "unsub", Description: "Remove a subscription: /unsub <id>"},
		{Command: "pause", Description: "Pause or resume a subscription: /pause <id>"},
		{Command: "test", Description: "Dry-run a subscription: /test <id>"},
		{Command: "status", Description: "Active downloads with progress"},
		{Command: "help", Description: "How to use the bot"},
	}
}

// handleCommand routes a "/command args" message to its handler.
func (h *Handlers) handleCommand(ctx context.Context, api telegramAPI, text string) {
	cmd, args := splitCommand(text)
	switch cmd {
	case "/sub":
		h.cmdSub(ctx, api, args)
	case "/subs":
		h.cmdSubs(ctx, api)
	case "/unsub":
		h.cmdUnsub(ctx, api, args)
	case "/pause":
		h.cmdPause(ctx, api, args)
	case "/test":
		h.cmdTest(ctx, api, args)
	case "/status":
		h.cmdStatus(ctx, api)
	case "/help", "/start":
		h.reply(ctx, api, helpText)
	default:
		h.reply(ctx, api, "Unknown command. Send plain text to search, or see /help.")
	}
}

// splitCommand separates the command word from its arguments — cutting on ANY
// whitespace, so "/sub\nquery" typed on a phone works — and strips an
// optional @botname suffix ("/subs@my_bot" → "/subs").
func splitCommand(text string) (cmd, args string) {
	cmd = text
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		cmd = text[:i]
		args = text[i:]
	}
	if at := strings.IndexByte(cmd, '@'); at > 0 {
		cmd = cmd[:at]
	}
	return cmd, strings.TrimSpace(args)
}

// cmdSub creates a subscription from "<query> | <filters>". Only the first
// "|" separates query from filters, so regex filters may contain "|".
func (h *Handlers) cmdSub(ctx context.Context, api telegramAPI, args string) {
	queryPart, filterPart, _ := strings.Cut(args, "|")
	query := strings.TrimSpace(queryPart)
	if query == "" {
		h.reply(ctx, api, subUsage)
		return
	}
	f, err := filter.Parse(filterPart)
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Bad filter: %v\n\n%s", err, subUsage))
		return
	}

	sub, err := h.subs.CreateSubscription(ctx, store.Subscription{
		Query:     query,
		Include:   f.Include,
		Exclude:   f.Exclude,
		MinSizeMB: f.MinSizeMB,
		MaxSizeMB: f.MaxSizeMB,
	})
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Failed to save subscription: %v", err))
		return
	}

	msg := fmt.Sprintf("✅ Subscribed #%d: «%s»", sub.ID, sub.Query)
	if fs := f.String(); fs != "" {
		msg += "\nFilters: " + fs
	}
	h.reply(ctx, api, msg)
}

// cmdSubs lists every subscription with filters, state, and stats.
func (h *Handlers) cmdSubs(ctx context.Context, api telegramAPI) {
	subs, err := h.subs.ListSubscriptions(ctx)
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Failed to list subscriptions: %v", err))
		return
	}
	if len(subs) == 0 {
		h.reply(ctx, api, "No subscriptions yet. Create one with /sub <query> | <filters>.")
		return
	}

	var b strings.Builder
	b.WriteString("📋 Subscriptions:\n")
	for _, sub := range subs {
		b.WriteString("\n" + subscriptionLine(sub))
	}
	h.reply(ctx, api, b.String())
}

// subscriptionLine renders one /subs entry.
func subscriptionLine(sub store.Subscription) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d «%s»", sub.ID, sub.Query)
	if sub.Paused {
		b.WriteString(" — ⏸ paused")
	}
	if fs := subFilter(sub).String(); fs != "" {
		b.WriteString("\n    filters: " + fs)
	}
	lastCheck := "never"
	if !sub.LastCheckedAt.IsZero() {
		lastCheck = sub.LastCheckedAt.UTC().Format(lastCheckedFormat)
	}
	fmt.Fprintf(&b, "\n    grabs: %d · last check: %s", sub.Grabs, lastCheck)
	return b.String()
}

// cmdUnsub deletes the subscription with the given id.
func (h *Handlers) cmdUnsub(ctx context.Context, api telegramAPI, args string) {
	id, ok := parseID(args)
	if !ok {
		h.reply(ctx, api, "Usage: /unsub <id> — see /subs for ids.")
		return
	}
	switch err := h.subs.DeleteSubscription(ctx, id); {
	case errors.Is(err, store.ErrNotFound):
		h.reply(ctx, api, fmt.Sprintf("Subscription #%d not found — see /subs.", id))
	case err != nil:
		h.reply(ctx, api, fmt.Sprintf("Failed to remove subscription #%d: %v", id, err))
	default:
		h.reply(ctx, api, fmt.Sprintf("🗑 Removed subscription #%d.", id))
	}
}

// cmdPause toggles a subscription between paused and active.
func (h *Handlers) cmdPause(ctx context.Context, api telegramAPI, args string) {
	id, ok := parseID(args)
	if !ok {
		h.reply(ctx, api, "Usage: /pause <id> — see /subs for ids.")
		return
	}
	sub, err := h.subs.GetSubscription(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		h.reply(ctx, api, fmt.Sprintf("Subscription #%d not found — see /subs.", id))
		return
	}
	if err == nil {
		err = h.subs.SetSubscriptionPaused(ctx, id, !sub.Paused)
	}
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Failed to update subscription #%d: %v", id, err))
		return
	}
	if sub.Paused {
		h.reply(ctx, api, fmt.Sprintf("▶️ Resumed subscription #%d «%s».", id, sub.Query))
	} else {
		h.reply(ctx, api, fmt.Sprintf("⏸ Paused subscription #%d «%s».", id, sub.Query))
	}
}

// cmdTest dry-runs a subscription: it searches and filters like the engine
// would, but downloads nothing and leaves the seen-table untouched (the
// SubscriptionStore interface has no seen methods at all).
func (h *Handlers) cmdTest(ctx context.Context, api telegramAPI, args string) {
	id, ok := parseID(args)
	if !ok {
		h.reply(ctx, api, "Usage: /test <id> — see /subs for ids.")
		return
	}
	sub, err := h.subs.GetSubscription(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		h.reply(ctx, api, fmt.Sprintf("Subscription #%d not found — see /subs.", id))
		return
	}
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Failed to load subscription #%d: %v", id, err))
		return
	}

	releases, err := h.searcher.Search(ctx, sub.Query)
	if err != nil {
		h.reply(ctx, api, "Search failed: "+err.Error())
		return
	}

	f := subFilter(sub)
	var matched []prowlarr.Release
	for _, r := range releases {
		if f.Match(r.Title, r.Size) {
			matched = append(matched, r)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🧪 Dry run #%d «%s»: %d of %d results match.",
		sub.ID, sub.Query, len(matched), len(releases))
	shown := min(len(matched), maxDryRunLines)
	for _, r := range matched[:shown] {
		fmt.Fprintf(&b, "\n• %s · %dS · %s", humanSize(r.Size), r.Seeders, r.Title)
	}
	if rest := len(matched) - shown; rest > 0 {
		fmt.Fprintf(&b, "\n…and %d more", rest)
	}
	if len(matched) > 0 {
		b.WriteString("\n\nNothing was downloaded.")
	}
	h.reply(ctx, api, b.String())
}

// cmdStatus lists the bot's active downloads with live Transmission progress.
func (h *Handlers) cmdStatus(ctx context.Context, api telegramAPI) {
	downloads, err := h.subs.ActiveDownloads(ctx)
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Failed to list downloads: %v", err))
		return
	}
	if len(downloads) == 0 {
		h.reply(ctx, api, "No active downloads.")
		return
	}

	statuses, err := h.trans.Active(ctx)
	if err != nil {
		h.reply(ctx, api, fmt.Sprintf("Transmission unreachable: %v", err))
		return
	}
	// Hash matching is case-insensitive, same as the completion watcher:
	// Transmission's hash casing is not guaranteed to match what was stored.
	byHash := make(map[string]transmission.TorrentStatus, len(statuses))
	for _, s := range statuses {
		byHash[strings.ToLower(s.Hash)] = s
	}

	var b strings.Builder
	b.WriteString("⬇️ Active downloads:\n")
	for _, dl := range downloads {
		b.WriteString("\n• " + dl.Title + "\n    ")
		st, ok := byHash[strings.ToLower(dl.Hash)]
		switch {
		case !ok:
			b.WriteString("not in Transmission (removed externally?)")
		case st.Done:
			b.WriteString(progressBar(1) + " 100% ✅")
		default:
			fmt.Fprintf(&b, "%s %.0f%% · %s/s · ETA %s",
				progressBar(st.Percent), st.Percent*100, humanSize(st.Rate), humanETA(st.ETA))
		}
	}
	h.reply(ctx, api, b.String())
}

// subFilter rebuilds the release filter a subscription was created with.
func subFilter(sub store.Subscription) filter.Filter {
	return filter.Filter{
		Include:   sub.Include,
		Exclude:   sub.Exclude,
		MinSizeMB: sub.MinSizeMB,
		MaxSizeMB: sub.MaxSizeMB,
	}
}

// parseID parses a positive numeric command argument.
func parseID(args string) (int64, bool) {
	id, err := strconv.ParseInt(args, 10, 64)
	return id, err == nil && id > 0
}

// barCells is how many cells a progress bar has.
const barCells = 10

// progressBar renders a fraction (0..1, clamped) as "▰▰▰▰▱▱▱▱▱▱"; a cell
// fills only once fully earned, so early progress is never overstated.
func progressBar(fraction float64) string {
	filled := int(fraction * barCells)
	filled = max(0, min(filled, barCells))
	return strings.Repeat("▰", filled) + strings.Repeat("▱", barCells-filled)
}

// humanETA renders a Transmission ETA; negative means unknown.
func humanETA(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}
