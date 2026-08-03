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
	// FindDownloads looks up what the bot already did with the given releases,
	// matched by info hash or exact title.
	FindDownloads(ctx context.Context, hashes, titles []string) ([]store.Download, error)
	// RecentCompleted returns the most recently finished downloads.
	RecentCompleted(ctx context.Context, limit int) ([]store.Download, error)
	// AddDownload records a torrent handed to Transmission so the completion
	// watcher can notify when it finishes.
	AddDownload(ctx context.Context, hash, title, source string) error
	// CancelDownload closes out a download the user rejected, keeping it out of
	// both the active list and the completed history.
	CancelDownload(ctx context.Context, hash string) error
}

var _ SubscriptionStore = (*store.Store)(nil)

// maxDryRunLines caps how many matching releases /test lists in one message.
const maxDryRunLines = 10

// maxCompletedLines caps how much finished-download history /status shows.
// Done rows are never deleted, so this is what keeps the command readable on a
// phone years into an install.
const maxCompletedLines = 10

// completedFormat renders the added-at timestamp in /status history.
const completedFormat = "2006-01-02 15:04"

// lastCheckedFormat renders subscription check timestamps in /subs.
const lastCheckedFormat = "2006-01-02 15:04"

const subUsage = "Usage: /sub <query> | <filters>\n" +
	"Example: /sub space show 2026 | rus, 1080p, x265, -720p, >1gb\n" +
	"Add the backlog filter to also grab what is already on the tracker."

const helpText = `🔍 Send any text to search for torrents; tap a result to download it.
Tap 🔔 under the results to subscribe to that exact search.
Tap ℹ️<number> to see that release in full first — every detail line
untruncated, what the tracker says about it, and the list of files inside.
A result the bot already grabbed is marked: ⬇️ downloading · ✅ downloaded.

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
  >1gb / <30gb — size bounds (mb or gb)
  backlog — also grab what is already on the tracker

A subscription downloads new releases by itself and says so in the chat, with
a 🗑 button to delete one you did not want. By default it only takes releases
published after you created it; tap ⬇️ for anything already listed.`

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

// handleCommand routes a "/command args" message to its handler. chatID is
// the chat the command came from, and where its answer goes.
func (h *Handlers) handleCommand(ctx context.Context, api telegramAPI, chatID int64, text string) {
	cmd, args := splitCommand(text)
	switch cmd {
	case "/sub":
		h.cmdSub(ctx, api, chatID, args)
	case "/subs":
		h.cmdSubs(ctx, api, chatID)
	case "/unsub":
		h.cmdUnsub(ctx, api, chatID, args)
	case "/pause":
		h.cmdPause(ctx, api, chatID, args)
	case "/test":
		h.cmdTest(ctx, api, chatID, args)
	case "/status":
		h.cmdStatus(ctx, api, chatID)
	case "/help", "/start":
		h.reply(ctx, api, chatID, helpText)
	default:
		h.reply(ctx, api, chatID, "Unknown command. Send plain text to search, or see /help.")
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
func (h *Handlers) cmdSub(ctx context.Context, api telegramAPI, chatID int64, args string) {
	queryPart, filterPart, _ := strings.Cut(args, "|")
	query := strings.TrimSpace(queryPart)
	if query == "" {
		h.reply(ctx, api, chatID, subUsage)
		return
	}
	// backlog is a property of the subscription rather than of the title, so it
	// has to come out before Parse — left in, it would become a required
	// substring and the subscription would match nothing at all.
	filterPart, wantBacklog := cutBacklogToken(filterPart)
	f, err := filter.Parse(filterPart)
	if err != nil {
		h.reply(ctx, api, chatID, fmt.Sprintf("Bad filter: %v\n\n%s", err, subUsage))
		return
	}

	cutoff := time.Now().UTC()
	if wantBacklog {
		cutoff = time.Time{}
	}
	sub, err := h.subs.CreateSubscription(ctx, store.Subscription{
		Query:     query,
		Include:   f.Include,
		Exclude:   f.Exclude,
		MinSizeMB: f.MinSizeMB,
		MaxSizeMB: f.MaxSizeMB,
		CutoffAt:  cutoff,
	})
	if err != nil {
		h.reply(ctx, api, chatID, fmt.Sprintf("Failed to save subscription: %v", err))
		return
	}

	msg := fmt.Sprintf("✅ Subscribed #%d: «%s»", sub.ID, sub.Query)
	if fs := f.String(); fs != "" {
		msg += "\nFilters: " + fs
	}
	msg += "\n" + cutoffLine(sub)
	h.reply(ctx, api, chatID, msg)
}

// backlogToken opts a subscription out of the publish-date cutoff, so it grabs
// what is already on the tracker as well as what turns up later.
const backlogToken = "backlog"

// cutBacklogToken removes the backlog token from a filter string, reporting
// whether it was there. Matching is case-insensitive and per token, so a
// release title containing the word is untouched.
func cutBacklogToken(filters string) (rest string, found bool) {
	kept := make([]string, 0, strings.Count(filters, ",")+1)
	for _, tok := range strings.Split(filters, ",") {
		if strings.EqualFold(strings.TrimSpace(tok), backlogToken) {
			found = true
			continue
		}
		kept = append(kept, tok)
	}
	return strings.Join(kept, ","), found
}

// cutoffLine states what a subscription will and will not reach back for. It
// is the one visible sign of the publish-date cutoff, so it is spelled out on
// creation and repeated in /subs rather than left to be discovered.
func cutoffLine(sub store.Subscription) string {
	if sub.CutoffAt.IsZero() {
		return "Grabbing everything that matches, including older releases."
	}
	return "Grabbing releases published from " + sub.CutoffAt.UTC().Format(cutoffFormat) + " onward."
}

// cutoffFormat renders the cutoff date; the day is the useful precision here.
const cutoffFormat = "2006-01-02"

// cmdSubs lists every subscription with filters, state, and stats.
func (h *Handlers) cmdSubs(ctx context.Context, api telegramAPI, chatID int64) {
	subs, err := h.subs.ListSubscriptions(ctx)
	if err != nil {
		h.reply(ctx, api, chatID, fmt.Sprintf("Failed to list subscriptions: %v", err))
		return
	}
	if len(subs) == 0 {
		h.reply(ctx, api, chatID, "No subscriptions yet. Create one with /sub <query> | <filters>.")
		return
	}

	var b strings.Builder
	b.WriteString("📋 Subscriptions:\n")
	for _, sub := range subs {
		b.WriteString("\n" + subscriptionLine(sub))
	}
	h.reply(ctx, api, chatID, b.String())
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
	if sub.CutoffAt.IsZero() {
		b.WriteString("\n    from: any date (backlog included)")
	} else {
		b.WriteString("\n    from: " + sub.CutoffAt.UTC().Format(cutoffFormat))
	}
	return b.String()
}

// cmdUnsub deletes the subscription with the given id.
func (h *Handlers) cmdUnsub(ctx context.Context, api telegramAPI, chatID int64, args string) {
	id, ok := parseID(args)
	if !ok {
		h.reply(ctx, api, chatID, "Usage: /unsub <id> — see /subs for ids.")
		return
	}
	switch err := h.subs.DeleteSubscription(ctx, id); {
	case errors.Is(err, store.ErrNotFound):
		h.reply(ctx, api, chatID, fmt.Sprintf("Subscription #%d not found — see /subs.", id))
	case err != nil:
		h.reply(ctx, api, chatID, fmt.Sprintf("Failed to remove subscription #%d: %v", id, err))
	default:
		h.reply(ctx, api, chatID, fmt.Sprintf("🗑 Removed subscription #%d.", id))
	}
}

// cmdPause toggles a subscription between paused and active.
func (h *Handlers) cmdPause(ctx context.Context, api telegramAPI, chatID int64, args string) {
	id, ok := parseID(args)
	if !ok {
		h.reply(ctx, api, chatID, "Usage: /pause <id> — see /subs for ids.")
		return
	}
	sub, err := h.subs.GetSubscription(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		h.reply(ctx, api, chatID, fmt.Sprintf("Subscription #%d not found — see /subs.", id))
		return
	}
	if err == nil {
		err = h.subs.SetSubscriptionPaused(ctx, id, !sub.Paused)
	}
	if err != nil {
		h.reply(ctx, api, chatID, fmt.Sprintf("Failed to update subscription #%d: %v", id, err))
		return
	}
	if sub.Paused {
		h.reply(ctx, api, chatID, fmt.Sprintf("▶️ Resumed subscription #%d «%s».", id, sub.Query))
	} else {
		h.reply(ctx, api, chatID, fmt.Sprintf("⏸ Paused subscription #%d «%s».", id, sub.Query))
	}
}

// cmdTest dry-runs a subscription: it searches and filters like the engine
// would, but downloads nothing and leaves the seen-table untouched (the
// SubscriptionStore interface has no seen methods at all).
func (h *Handlers) cmdTest(ctx context.Context, api telegramAPI, chatID int64, args string) {
	id, ok := parseID(args)
	if !ok {
		h.reply(ctx, api, chatID, "Usage: /test <id> — see /subs for ids.")
		return
	}
	sub, err := h.subs.GetSubscription(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		h.reply(ctx, api, chatID, fmt.Sprintf("Subscription #%d not found — see /subs.", id))
		return
	}
	if err != nil {
		h.reply(ctx, api, chatID, fmt.Sprintf("Failed to load subscription #%d: %v", id, err))
		return
	}

	// Same slow search as a plain query, so the same ack — but left standing
	// rather than edited: the dry-run answer below is a list that h.reply may
	// have to split across several messages.
	h.ackSearching(ctx, api, chatID, sub.Query)

	releases, err := h.searcher.Search(ctx, sub.Query)
	if err != nil {
		h.reply(ctx, api, chatID, "Search failed: "+err.Error())
		return
	}

	f := subFilter(sub)
	var matched []prowlarr.Release
	for _, r := range releases {
		if f.Match(r.Title, r.Size, r.PublishDate) {
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
	h.reply(ctx, api, chatID, b.String())
}

// cmdStatus lists the bot's active downloads with live Transmission progress,
// followed by what it recently finished.
func (h *Handlers) cmdStatus(ctx context.Context, api telegramAPI, chatID int64) {
	active, err := h.subs.ActiveDownloads(ctx)
	if err != nil {
		h.reply(ctx, api, chatID, fmt.Sprintf("Failed to list downloads: %v", err))
		return
	}
	completed, err := h.subs.RecentCompleted(ctx, maxCompletedLines)
	if err != nil {
		h.reply(ctx, api, chatID, fmt.Sprintf("Failed to list downloads: %v", err))
		return
	}
	if len(active) == 0 && len(completed) == 0 {
		h.reply(ctx, api, chatID, "No downloads yet.")
		return
	}

	var sections []string
	if len(active) > 0 {
		sections = append(sections, h.activeSection(ctx, active))
	}
	if len(completed) > 0 {
		sections = append(sections, completedSection(completed))
	}
	h.reply(ctx, api, chatID, strings.Join(sections, "\n\n"))
}

// activeSection renders the running downloads with live Transmission progress.
// A Transmission outage costs this section its progress figures only — the
// history rendered beside it comes from the store and stays readable.
func (h *Handlers) activeSection(ctx context.Context, active []store.Download) string {
	statuses, err := h.trans.Active(ctx)
	if err != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "⬇️ Active downloads (Transmission unreachable: %v):\n", err)
		for _, dl := range active {
			b.WriteString("\n• " + dl.Title)
		}
		return b.String()
	}
	// Hash matching is case-insensitive, same as the completion watcher:
	// Transmission's hash casing is not guaranteed to match what was stored.
	byHash := make(map[string]transmission.TorrentStatus, len(statuses))
	for _, s := range statuses {
		byHash[strings.ToLower(s.Hash)] = s
	}

	var b strings.Builder
	b.WriteString("⬇️ Active downloads:\n")
	for _, dl := range active {
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
	return b.String()
}

// completedSection renders the recent history. The store records when a
// download was added, not when it finished, so the line says so rather than
// implying a completion time it does not have.
func completedSection(completed []store.Download) string {
	var b strings.Builder
	b.WriteString("✅ Recently completed:\n")
	for _, dl := range completed {
		fmt.Fprintf(&b, "\n• %s — added %s", dl.Title, dl.AddedAt.UTC().Format(completedFormat))
	}
	return b.String()
}

// subFilter rebuilds the release filter a subscription was created with. The
// cutoff rides along so that /test judges a release exactly as the engine
// would; a dry run that reported matches the engine then skipped would be
// worse than no dry run at all.
func subFilter(sub store.Subscription) filter.Filter {
	return filter.Filter{
		Include:   sub.Include,
		Exclude:   sub.Exclude,
		MinSizeMB: sub.MinSizeMB,
		MaxSizeMB: sub.MaxSizeMB,
		Since:     sub.CutoffAt,
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
