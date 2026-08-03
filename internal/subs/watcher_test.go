package subs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// --- fakes ---

// fakeDLStore implements DownloadStore in memory. Status changes persist
// across Watcher instances, which stands in for bot restarts in tests.
type fakeDLStore struct {
	mu          sync.Mutex
	order       []string // hashes in insertion order
	rows        map[string]*store.Download
	activeErr   error
	completeErr error
	listedC     chan struct{} // signaled on every ActiveDownloads call
}

func newFakeDLStore(dls ...store.Download) *fakeDLStore {
	f := &fakeDLStore{
		rows:    map[string]*store.Download{},
		listedC: make(chan struct{}, 64),
	}
	for _, dl := range dls {
		if dl.Status == "" {
			dl.Status = store.StatusActive
		}
		f.order = append(f.order, dl.Hash)
		row := dl
		f.rows[dl.Hash] = &row
	}
	return f
}

func (f *fakeDLStore) ActiveDownloads(context.Context) ([]store.Download, error) {
	f.mu.Lock()
	err := f.activeErr
	var dls []store.Download
	for _, hash := range f.order {
		if dl := f.rows[hash]; dl.Status == store.StatusActive {
			dls = append(dls, *dl)
		}
	}
	f.mu.Unlock()

	select {
	case f.listedC <- struct{}{}:
	default:
	}
	if err != nil {
		return nil, err
	}
	return dls, nil
}

func (f *fakeDLStore) CompleteDownload(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeErr != nil {
		return f.completeErr
	}
	dl, ok := f.rows[hash]
	if !ok {
		return store.ErrNotFound
	}
	dl.Status = store.StatusDone
	return nil
}

func (f *fakeDLStore) status(hash string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[hash].Status
}

var _ DownloadStore = (*fakeDLStore)(nil)

// --- helpers ---

type watchEnv struct {
	store   *fakeDLStore
	trans   *fakeTrans
	notify  *fakeNotifier
	watcher *Watcher
}

func newWatchEnv(t *testing.T, dls ...store.Download) *watchEnv {
	t.Helper()
	env := &watchEnv{
		store:  newFakeDLStore(dls...),
		trans:  &fakeTrans{},
		notify: &fakeNotifier{},
	}
	// One-hour interval: only explicit Check calls (or the immediate first
	// check in Run) execute during a test.
	env.watcher = NewWatcher(env.store, env.trans, env.notify, time.Hour,
		slog.New(slog.DiscardHandler))
	return env
}

func doneStatus(hash, name string) transmission.TorrentStatus {
	return transmission.TorrentStatus{Hash: hash, Name: name, Percent: 1, Done: true}
}

// --- completion flow ---

func TestCheckFinishedDownloadNotifiesOnceAndMarksDone(t *testing.T) {
	env := newWatchEnv(t,
		store.Download{Hash: "aaa", Title: "Космос 2026 Серия 5 [1080p, Rus]"},
		store.Download{Hash: "bbb", Title: "Ubuntu 26.04 LTS"},
	)
	env.trans.statuses = []transmission.TorrentStatus{
		doneStatus("aaa", "f1"),
		{Hash: "bbb", Name: "ubuntu", Percent: 0.4},
	}

	env.watcher.Check(context.Background())

	msgs := env.notify.messages()
	if len(msgs) != 1 {
		t.Fatalf("notifications = %q, want exactly 1", msgs)
	}
	if !strings.Contains(msgs[0], "✅") || !strings.Contains(msgs[0], "Космос 2026") {
		t.Errorf("notification %q must contain ✅ and the download title", msgs[0])
	}
	if got := env.store.status("aaa"); got != store.StatusDone {
		t.Errorf("finished download status = %q, want %q", got, store.StatusDone)
	}
	if got := env.store.status("bbb"); got != store.StatusActive {
		t.Errorf("in-progress download status = %q, want %q", got, store.StatusActive)
	}

	// Further cycles with unchanged Transmission state stay quiet.
	env.watcher.Check(context.Background())
	env.watcher.Check(context.Background())
	if got := env.notify.messages(); len(got) != 1 {
		t.Errorf("notifications after extra cycles = %q, want still exactly 1", got)
	}
}

func TestCheckDoneRowsNeverRenotifyAcrossRestarts(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "aaa", Title: "movie"})
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("aaa", "movie")}

	env.watcher.Check(context.Background())
	if got := env.notify.messages(); len(got) != 1 {
		t.Fatalf("notifications = %q, want exactly 1 before restart", got)
	}

	// "Restart": a fresh watcher over the same persisted store and the same
	// still-seeding torrent must not notify again.
	notify2 := &fakeNotifier{}
	w2 := NewWatcher(env.store, env.trans, notify2, time.Hour, slog.New(slog.DiscardHandler))
	w2.Check(context.Background())

	if got := notify2.messages(); len(got) != 0 {
		t.Errorf("restarted watcher re-notified: %q", got)
	}
}

func TestCheckNotificationFallsBackToTransmissionName(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "aaa"}) // no title recorded
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("aaa", "Name From Transmission")}

	env.watcher.Check(context.Background())

	msgs := env.notify.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "Name From Transmission") {
		t.Errorf("notifications = %q, want one containing the Transmission name", msgs)
	}
}

func TestCheckHashMatchingIsCaseInsensitive(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "ABCDEF", Title: "movie"})
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("abcdef", "movie")}

	env.watcher.Check(context.Background())

	if got := env.store.status("ABCDEF"); got != store.StatusDone {
		t.Errorf("status = %q, want %q despite hash case difference", got, store.StatusDone)
	}
	if got := env.notify.messages(); len(got) != 1 {
		t.Errorf("notifications = %q, want exactly 1", got)
	}
}

// --- failure semantics ---

func TestCheckTransmissionUnreachableSkipsCycleSilently(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "aaa", Title: "movie"})
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("aaa", "movie")}
	env.trans.activeErr = errors.New("connection refused")

	env.watcher.Check(context.Background())

	if got := env.notify.messages(); len(got) != 0 {
		t.Errorf("unreachable Transmission produced notifications: %q", got)
	}
	if got := env.store.status("aaa"); got != store.StatusActive {
		t.Errorf("status = %q, want still %q after a skipped cycle", got, store.StatusActive)
	}

	// Transmission recovers: the next cycle completes normally.
	env.trans.mu.Lock()
	env.trans.activeErr = nil
	env.trans.mu.Unlock()
	env.watcher.Check(context.Background())

	if got := env.notify.messages(); len(got) != 1 {
		t.Errorf("notifications after recovery = %q, want exactly 1", got)
	}
	if got := env.store.status("aaa"); got != store.StatusDone {
		t.Errorf("status after recovery = %q, want %q", got, store.StatusDone)
	}
}

func TestCheckRemovedTorrentMarkedDoneWithoutNotification(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "gone", Title: "removed externally"})
	env.trans.statuses = []transmission.TorrentStatus{
		{Hash: "other", Name: "unrelated", Percent: 0.2},
	}

	env.watcher.Check(context.Background())

	if got := env.notify.messages(); len(got) != 0 {
		t.Errorf("externally removed torrent triggered notifications: %q", got)
	}
	if got := env.store.status("gone"); got != store.StatusDone {
		t.Errorf("status = %q, want %q so the row stops being polled", got, store.StatusDone)
	}
}

func TestCheckNotifyFailureKeepsDownloadActiveForRetry(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "aaa", Title: "movie"})
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("aaa", "movie")}
	env.notify.setErr(errors.New("telegram down"))

	env.watcher.Check(context.Background())

	if got := env.store.status("aaa"); got != store.StatusActive {
		t.Fatalf("status = %q after failed notification, want %q for retry", got, store.StatusActive)
	}

	// Telegram recovers: exactly one notification is delivered, then done.
	env.notify.setErr(nil)
	env.watcher.Check(context.Background())

	if got := env.notify.messages(); len(got) != 1 {
		t.Errorf("notifications after recovery = %q, want exactly 1", got)
	}
	if got := env.store.status("aaa"); got != store.StatusDone {
		t.Errorf("status after recovery = %q, want %q", got, store.StatusDone)
	}
}

func TestCheckCompleteFailureAfterNotifyRetriesNextCycle(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "aaa", Title: "movie"})
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("aaa", "movie")}
	env.store.mu.Lock()
	env.store.completeErr = errors.New("database locked")
	env.store.mu.Unlock()

	env.watcher.Check(context.Background())

	// The notification went out but the done-flip failed: the row stays
	// active so the next cycle retries the flip.
	if got := env.notify.messages(); len(got) != 1 {
		t.Fatalf("notifications = %q, want exactly 1", got)
	}
	if got := env.store.status("aaa"); got != store.StatusActive {
		t.Fatalf("status = %q after failed CompleteDownload, want %q", got, store.StatusActive)
	}

	// The store recovers: the documented worst case is one duplicate
	// notification, after which the row settles as done.
	env.store.mu.Lock()
	env.store.completeErr = nil
	env.store.mu.Unlock()
	env.watcher.Check(context.Background())

	if got := env.notify.messages(); len(got) != 2 {
		t.Errorf("notifications after recovery = %q, want 2 (one duplicate, then done)", got)
	}
	if got := env.store.status("aaa"); got != store.StatusDone {
		t.Errorf("status after recovery = %q, want %q", got, store.StatusDone)
	}

	// From here on the download is closed out for good.
	env.watcher.Check(context.Background())
	if got := env.notify.messages(); len(got) != 2 {
		t.Errorf("notifications after settling = %q, want still 2", got)
	}
}

func TestCheckRemovedTorrentCompleteFailureKeepsPolling(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "gone", Title: "removed externally"})
	env.trans.statuses = nil // torrent no longer in Transmission
	env.store.mu.Lock()
	env.store.completeErr = errors.New("database locked")
	env.store.mu.Unlock()

	env.watcher.Check(context.Background())

	// The close-out failed: no notification either way, and the row stays
	// active so the next cycle retries closing it.
	if got := env.notify.messages(); len(got) != 0 {
		t.Fatalf("externally removed torrent triggered notifications: %q", got)
	}
	if got := env.store.status("gone"); got != store.StatusActive {
		t.Fatalf("status = %q after failed close-out, want %q", got, store.StatusActive)
	}

	env.store.mu.Lock()
	env.store.completeErr = nil
	env.store.mu.Unlock()
	env.watcher.Check(context.Background())

	if got := env.store.status("gone"); got != store.StatusDone {
		t.Errorf("status after recovery = %q, want %q", got, store.StatusDone)
	}
	if got := env.notify.messages(); len(got) != 0 {
		t.Errorf("silent close-out produced notifications: %q", got)
	}
}

func TestCheckStoreFailureSkipsCycle(t *testing.T) {
	env := newWatchEnv(t, store.Download{Hash: "aaa", Title: "movie"})
	env.store.activeErr = errors.New("database locked")

	env.watcher.Check(context.Background()) // must not panic

	if got := env.trans.activeCount(); got != 0 {
		t.Errorf("polled Transmission %d time(s) despite store failure, want 0", got)
	}
	if got := env.notify.messages(); len(got) != 0 {
		t.Errorf("store failure produced notifications: %q", got)
	}
}

func TestCheckNoActiveDownloadsSkipsTransmissionPoll(t *testing.T) {
	env := newWatchEnv(t,
		store.Download{Hash: "old", Title: "already finished", Status: store.StatusDone},
	)

	env.watcher.Check(context.Background())

	if got := env.trans.activeCount(); got != 0 {
		t.Errorf("Active called %d time(s) with nothing to watch, want 0", got)
	}
}

// --- constructor + Run loop ---

func TestNewWatcherDefaults(t *testing.T) {
	w := NewWatcher(newFakeDLStore(), &fakeTrans{}, &fakeNotifier{}, 0, nil)
	if w.interval != DefaultWatchInterval {
		t.Errorf("interval = %v, want default %v", w.interval, DefaultWatchInterval)
	}
	if w.log == nil {
		t.Error("nil logger not replaced with a default")
	}
}

func TestWatcherRunFirstCheckIsImmediate(t *testing.T) {
	env := newWatchEnv(t) // one-hour interval: any check observed is the immediate one
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.watcher.Run(ctx)
		close(done)
	}()

	select {
	case <-env.store.listedC:
	case <-time.After(2 * time.Second):
		t.Fatal("no immediate first check within 2s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on context cancel")
	}
}

func TestWatcherRunChecksPeriodically(t *testing.T) {
	env := newWatchEnv(t)
	env.watcher.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.watcher.Run(ctx)
		close(done)
	}()

	// Immediate check plus at least two ticker fires.
	for i := 0; i < 3; i++ {
		select {
		case <-env.store.listedC:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d check(s) observed within 2s", i)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on context cancel")
	}
}

// A finished subscription grab still gets an undo: the user never chose it,
// and by the time it lands they may well not want it.
func TestFinishedSubscriptionGrabOffersUndo(t *testing.T) {
	env := newWatchEnv(t,
		store.Download{Hash: "aaa", Title: "Космос 2026 Серия 5 [1080p, Rus]", Source: "sub:3"},
	)
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("aaa", "kosmos")}

	env.watcher.Check(context.Background())

	hashes := env.notify.grabHashes()
	if len(hashes) != 1 || hashes[0] != "aaa" {
		t.Errorf("completion notification hashes = %v, want [aaa]", hashes)
	}
}

// A download the user picked out of search results is one they already decided
// they wanted, so its completion carries no undo.
func TestFinishedManualDownloadOffersNoUndo(t *testing.T) {
	env := newWatchEnv(t,
		store.Download{Hash: "aaa", Title: "Ubuntu 26.04 LTS", Source: "search"},
	)
	env.trans.statuses = []transmission.TorrentStatus{doneStatus("aaa", "ubuntu")}

	env.watcher.Check(context.Background())

	hashes := env.notify.grabHashes()
	if len(hashes) != 1 || hashes[0] != "" {
		t.Errorf("completion notification hashes = %v, want [\"\"] (a plain notification)", hashes)
	}
	if len(env.notify.messages()) != 1 {
		t.Errorf("notifications = %q, want exactly 1", env.notify.messages())
	}
}
