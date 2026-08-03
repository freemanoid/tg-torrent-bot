package subs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// DefaultWatchInterval is how often the watcher polls Transmission for
// download progress.
const DefaultWatchInterval = 30 * time.Second

// DownloadStore is the persistence surface the watcher uses; *store.Store
// implements it, tests fake it.
type DownloadStore interface {
	ActiveDownloads(ctx context.Context) ([]store.Download, error)
	CompleteDownload(ctx context.Context, hash string) error
}

var _ DownloadStore = (*store.Store)(nil)

// Watcher polls Transmission for the downloads the bot added and sends a
// one-time "finished" notification when each completes. Completion is
// persisted in the downloads table, so a restart never repeats notifications.
type Watcher struct {
	store    DownloadStore
	trans    transmission.Interface
	notifier Notifier
	interval time.Duration
	log      *slog.Logger
}

// NewWatcher wires a completion watcher. A non-positive interval falls back
// to DefaultWatchInterval; a nil log falls back to slog.Default().
func NewWatcher(st DownloadStore, trans transmission.Interface, notifier Notifier, interval time.Duration, log *slog.Logger) *Watcher {
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		store:    st,
		trans:    trans,
		notifier: notifier,
		interval: interval,
		log:      log,
	}
}

// Run checks immediately, then every interval, until ctx is canceled.
// It always returns nil: shutdown via context cancel is the normal exit.
func (w *Watcher) Run(ctx context.Context) error {
	runEvery(ctx, w.interval, w.Check)
	return nil
}

// Check runs one polling cycle. Failures are logged, never fatal: when the
// store or Transmission is unreachable the cycle is skipped silently and
// every still-active download is retried on the next one.
func (w *Watcher) Check(ctx context.Context) {
	dls, err := w.store.ActiveDownloads(ctx)
	if err != nil {
		w.log.Error("list active downloads", "error", err)
		return
	}
	if len(dls) == 0 {
		return // nothing to watch, don't bother Transmission
	}

	statuses, err := w.trans.Active(ctx)
	if err != nil {
		w.log.Warn("transmission poll failed, will retry", "error", err)
		return
	}
	byHash := make(map[string]transmission.TorrentStatus, len(statuses))
	for _, s := range statuses {
		byHash[strings.ToLower(s.Hash)] = s
	}

	for _, dl := range dls {
		st, ok := byHash[strings.ToLower(dl.Hash)]
		switch {
		case !ok:
			// Removed from Transmission externally: close the row out
			// without a notification so it stops being polled.
			w.log.Info("torrent gone from transmission, closing download", "hash", dl.Hash, "title", dl.Title)
			if err := w.store.CompleteDownload(ctx, dl.Hash); err != nil {
				w.log.Error("complete removed download", "hash", dl.Hash, "error", err)
			}
		case st.Done:
			w.finish(ctx, dl, st)
		}
	}
}

// finish announces one completed download, then flips its row to done. The
// notification goes first: if it fails the row stays active and the next
// cycle retries, so the user never silently misses a completion.
func (w *Watcher) finish(ctx context.Context, dl store.Download, st transmission.TorrentStatus) {
	title := dl.Title
	if title == "" {
		title = st.Name
	}
	// Only what a subscription grabbed on its own gets an undo: a download the
	// user chose from search results is one they already decided they wanted.
	msg := fmt.Sprintf("✅ Finished:\n%s", title)
	var err error
	if strings.HasPrefix(dl.Source, subSourcePrefix) {
		err = w.notifier.NotifyGrab(ctx, msg, dl.Hash)
	} else {
		err = w.notifier.Notify(ctx, msg)
	}
	if err != nil {
		w.log.Error("notify finished download", "hash", dl.Hash, "error", err)
		return
	}
	if err := w.store.CompleteDownload(ctx, dl.Hash); err != nil {
		// Worst case the next cycle notifies again; a failing store write is
		// exceptional enough to be loud about.
		w.log.Error("complete download", "hash", dl.Hash, "error", err)
	}
}
