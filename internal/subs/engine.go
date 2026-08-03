// Package subs runs the bot's background loops. The Engine re-runs every
// active subscription's search on a ticker, filters the results, auto-adds
// new matches to Transmission, and notifies the user via Telegram.
package subs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/filter"
	"github.com/freemanoid/tg-torrent-bot/internal/grab"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// DefaultTickInterval is the fallback tick period when NewEngine is handed a
// non-positive interval. Config validation rejects such values, but a zero
// interval must never reach time.NewTicker, which panics on it.
const DefaultTickInterval = 20 * time.Minute

// maxGrabsPerTick caps how many releases one subscription grabs in a single
// tick, so a brand-new filterless subscription trickles its backlog in over
// several ticks instead of flooding Transmission (and the chat) with a full
// search page at once. Releases over the cap stay unseen and are picked up on
// the following ticks.
const maxGrabsPerTick = 10

// subSourcePrefix marks a store.Download the subscription engine added, as
// opposed to one the user picked out of search results. The watcher reads it
// to decide whether a finished download is worth offering to undo.
const subSourcePrefix = "sub:"

// Store is the persistence surface the engine uses; *store.Store implements
// it, tests fake it.
type Store interface {
	ListSubscriptions(ctx context.Context) ([]store.Subscription, error)
	IsSeen(ctx context.Context, subID int64, guid string) (bool, error)
	MarkSeen(ctx context.Context, subID int64, guid, infoHash, title string) error
	IncrementGrabs(ctx context.Context, id int64) error
	SetLastChecked(ctx context.Context, id int64, at time.Time) error
	AddDownload(ctx context.Context, hash, title, source string) error
}

var _ Store = (*store.Store)(nil)

// Searcher is the Prowlarr surface the engine uses; *prowlarr.Client
// implements it, tests fake it.
type Searcher interface {
	Search(ctx context.Context, query string) ([]prowlarr.Release, error)
	FetchTorrent(ctx context.Context, downloadURL string) ([]byte, error)
}

var _ Searcher = (*prowlarr.Client)(nil)

// Notifier delivers a message to the user (in production: a Telegram message
// to every allowed chat).
type Notifier interface {
	Notify(ctx context.Context, text string) error
	// NotifyGrab delivers a message about a torrent the bot added on its own,
	// alongside a way to reject it. hash is the info hash Transmission
	// confirmed, which is all the rejection needs to act on — how that choice
	// is offered is the notifier's business, not the engine's.
	NotifyGrab(ctx context.Context, text, hash string) error
}

// Engine periodically checks all active subscriptions and grabs new matches.
type Engine struct {
	store    Store
	searcher Searcher
	trans    transmission.Interface
	notifier Notifier
	interval time.Duration
	log      *slog.Logger
}

// NewEngine wires a subscription engine. interval is the tick period
// (config.SubInterval); a non-positive interval falls back to
// DefaultTickInterval. A nil log falls back to slog.Default().
func NewEngine(st Store, searcher Searcher, trans transmission.Interface, notifier Notifier, interval time.Duration, log *slog.Logger) *Engine {
	if interval <= 0 {
		interval = DefaultTickInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		store:    st,
		searcher: searcher,
		trans:    trans,
		notifier: notifier,
		interval: interval,
		log:      log,
	}
}

// Run ticks immediately, then every interval, until ctx is canceled.
// It always returns nil: shutdown via context cancel is the normal exit.
func (e *Engine) Run(ctx context.Context) error {
	runEvery(ctx, e.interval, e.Tick)
	return nil
}

// runEvery calls fn immediately, then every interval, until ctx is canceled.
// It is the shared ticker loop behind Engine.Run and Watcher.Run.
func runEvery(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	fn(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn(ctx)
		}
	}
}

// Tick runs one pass over all subscriptions. Failures are logged, never
// fatal: a sub whose search fails is retried wholesale on the next tick, and
// one sub's failure does not block the others.
func (e *Engine) Tick(ctx context.Context) {
	subs, err := e.store.ListSubscriptions(ctx)
	if err != nil {
		e.log.Error("list subscriptions", "error", err)
		return
	}
	for _, sub := range subs {
		if sub.Paused {
			continue
		}
		if err := e.checkSub(ctx, sub); err != nil {
			// Same class as the watcher's Transmission poll failure: a
			// retryable external-service error, warned at the same severity.
			e.log.Warn("subscription check failed, will retry", "sub", sub.ID, "query", sub.Query, "error", err)
		}
	}
}

// checkSub searches one subscription and grabs every new matching release.
// When the search itself fails nothing is marked seen and last-checked stays
// untouched, so the next tick retries from scratch.
func (e *Engine) checkSub(ctx context.Context, sub store.Subscription) error {
	releases, err := e.searcher.Search(ctx, sub.Query)
	if err != nil {
		return err
	}

	f := filter.Filter{
		Include:   sub.Include,
		Exclude:   sub.Exclude,
		MinSizeMB: sub.MinSizeMB,
		MaxSizeMB: sub.MaxSizeMB,
		Since:     sub.CutoffAt,
	}
	// A subscription that has never been checked is seeing the tracker's whole
	// back catalogue at once; from the second tick on, everything it finds is
	// genuinely new to it. That distinction is what makes undated releases
	// safe to take later — see holdUndated.
	firstTick := sub.LastCheckedAt.IsZero()
	grabbed := 0
	for _, r := range releases {
		if !f.Match(r.Title, r.Size, r.PublishDate) {
			continue
		}
		if grabbed >= maxGrabsPerTick {
			e.log.Info("per-tick grab cap reached, remaining matches deferred to next tick",
				"sub", sub.ID, "cap", maxGrabsPerTick)
			break
		}
		seen, err := e.store.IsSeen(ctx, sub.ID, r.GUID)
		if err != nil {
			// Can't tell whether this was grabbed before — skip rather than
			// risk a duplicate; the next tick gets another chance.
			e.log.Error("seen lookup failed", "sub", sub.ID, "guid", r.GUID, "error", err)
			continue
		}
		if seen {
			continue
		}
		if holdUndated(sub, r, firstTick) {
			// Nothing was downloaded, so this must not count against the grab
			// cap; marking it seen is what keeps it out of the next tick.
			if err := e.store.MarkSeen(ctx, sub.ID, r.GUID, r.InfoHash, r.Title); err != nil {
				e.log.Error("mark undated release seen", "sub", sub.ID, "guid", r.GUID, "error", err)
			}
			continue
		}
		if e.grabRelease(ctx, sub, r) {
			grabbed++
		}
	}

	if err := e.store.SetLastChecked(ctx, sub.ID, time.Now().UTC()); err != nil {
		e.log.Warn("record last check", "sub", sub.ID, "error", err)
	}
	return nil
}

// holdUndated reports whether a release should be recorded as handled without
// being downloaded. It covers the one case the publish-date cutoff cannot
// judge: an indexer that reports no date at all. Failing those closed forever
// would leave a subscription silently grabbing nothing; taking them would hand
// over the whole backlog the cutoff exists to avoid. Holding them back on the
// first tick only splits the difference — the backlog is skipped once, and
// everything the subscription meets afterwards is new to it by construction.
func holdUndated(sub store.Subscription, r prowlarr.Release, firstTick bool) bool {
	return firstTick && !sub.CutoffAt.IsZero() && r.PublishDate.IsZero()
}

// grabRelease hands one release to Transmission and records it, reporting
// whether the add was confirmed. A failed add leaves the release unseen so
// the next tick retries it; only a confirmed add is marked seen, counted as a
// grab, and announced.
func (e *Engine) grabRelease(ctx context.Context, sub store.Subscription, r prowlarr.Release) bool {
	hash, err := grab.AddRelease(ctx, e.searcher, e.trans, r)
	if err != nil {
		e.log.Error("add release", "sub", sub.ID, "title", r.Title, "error", err)
		return false
	}
	if err := e.store.AddDownload(ctx, hash, r.Title, fmt.Sprintf("%s%d", subSourcePrefix, sub.ID)); err != nil {
		// The torrent is already downloading; only the completion
		// notification is lost.
		e.log.Error("record download", "hash", hash, "error", err)
	}
	if err := e.store.MarkSeen(ctx, sub.ID, r.GUID, r.InfoHash, r.Title); err != nil {
		// Worst case the release is re-added next tick and Transmission
		// dedupes it.
		e.log.Error("mark seen", "sub", sub.ID, "guid", r.GUID, "error", err)
	}
	if err := e.store.IncrementGrabs(ctx, sub.ID); err != nil {
		e.log.Warn("increment grabs", "sub", sub.ID, "error", err)
	}
	msg := fmt.Sprintf("📥 Sub #%d «%s» grabbed:\n%s", sub.ID, sub.Query, r.Title)
	// The hash Transmission confirmed, not the one the indexer advertised:
	// rejecting this download has to name the torrent Transmission actually
	// holds, and a magnet-only release may have advertised none at all.
	if err := e.notifier.NotifyGrab(ctx, msg, hash); err != nil {
		e.log.Error("notify grab", "sub", sub.ID, "error", err)
	}
	return true
}
