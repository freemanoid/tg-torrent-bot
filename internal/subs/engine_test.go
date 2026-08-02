package subs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// --- fakes ---

type fakeStore struct {
	mu          sync.Mutex
	subs        []store.Subscription
	listErr     error
	seen        map[string]bool // "subID|guid"
	seenTitles  map[string]string
	grabs       map[int64]int
	lastChecked map[int64]time.Time
	downloads   []store.Download

	isSeenErr      error
	markSeenErr    error
	addDLErr       error
	grabsErr       error
	lastCheckedErr error
}

func newFakeStore(subs ...store.Subscription) *fakeStore {
	return &fakeStore{
		subs:        subs,
		seen:        map[string]bool{},
		seenTitles:  map[string]string{},
		grabs:       map[int64]int{},
		lastChecked: map[int64]time.Time{},
	}
}

func seenKey(subID int64, guid string) string { return fmt.Sprintf("%d|%s", subID, guid) }

func (f *fakeStore) ListSubscriptions(context.Context) ([]store.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]store.Subscription(nil), f.subs...), nil
}

func (f *fakeStore) IsSeen(_ context.Context, subID int64, guid string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isSeenErr != nil {
		return false, f.isSeenErr
	}
	return f.seen[seenKey(subID, guid)], nil
}

func (f *fakeStore) MarkSeen(_ context.Context, subID int64, guid, _, title string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markSeenErr != nil {
		return f.markSeenErr
	}
	f.seen[seenKey(subID, guid)] = true
	f.seenTitles[seenKey(subID, guid)] = title
	return nil
}

func (f *fakeStore) IncrementGrabs(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grabsErr != nil {
		return f.grabsErr
	}
	f.grabs[id]++
	return nil
}

func (f *fakeStore) SetLastChecked(_ context.Context, id int64, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastCheckedErr != nil {
		return f.lastCheckedErr
	}
	f.lastChecked[id] = at
	return nil
}

// AddDownload mirrors the real store: adding a hash that already has an
// active row is a no-op.
func (f *fakeStore) AddDownload(_ context.Context, hash, title, source string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addDLErr != nil {
		return f.addDLErr
	}
	for _, dl := range f.downloads {
		if dl.Hash == hash {
			return nil
		}
	}
	f.downloads = append(f.downloads, store.Download{Hash: hash, Title: title, Source: source, Status: store.StatusActive})
	return nil
}

func (f *fakeStore) seenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

type fakeSearcher struct {
	mu        sync.Mutex
	results   map[string][]prowlarr.Release
	searchErr map[string]error
	torrents  map[string][]byte // by downloadURL
	fetchErr  error
	searches  []string
	searchedC chan string // signaled on every Search call
}

func newFakeSearcher() *fakeSearcher {
	return &fakeSearcher{
		results:   map[string][]prowlarr.Release{},
		searchErr: map[string]error{},
		torrents:  map[string][]byte{},
		searchedC: make(chan string, 64),
	}
}

func (f *fakeSearcher) Search(_ context.Context, query string) ([]prowlarr.Release, error) {
	f.mu.Lock()
	f.searches = append(f.searches, query)
	err := f.searchErr[query]
	res := append([]prowlarr.Release(nil), f.results[query]...)
	f.mu.Unlock()

	select {
	case f.searchedC <- query:
	default:
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (f *fakeSearcher) FetchTorrent(_ context.Context, downloadURL string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	meta, ok := f.torrents[downloadURL]
	if !ok {
		return nil, fmt.Errorf("no torrent at %s", downloadURL)
	}
	return meta, nil
}

func (f *fakeSearcher) searchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.searches)
}

func (f *fakeSearcher) searchedQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.searches...)
}

type fakeTrans struct {
	mu          sync.Mutex
	hash        string
	addErr      error
	torrents    [][]byte
	magnets     []string
	statuses    []transmission.TorrentStatus
	activeErr   error
	activeCalls int
}

func (f *fakeTrans) AddTorrent(_ context.Context, meta []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.torrents = append(f.torrents, meta)
	return f.hash, nil
}

func (f *fakeTrans) AddMagnet(_ context.Context, link string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.magnets = append(f.magnets, link)
	return f.hash, nil
}

func (f *fakeTrans) Active(context.Context) ([]transmission.TorrentStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeCalls++
	if f.activeErr != nil {
		return nil, f.activeErr
	}
	return append([]transmission.TorrentStatus(nil), f.statuses...), nil
}

func (f *fakeTrans) activeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.activeCalls
}

func (f *fakeTrans) addCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.torrents) + len(f.magnets)
}

type fakeNotifier struct {
	mu   sync.Mutex
	err  error
	msgs []string
}

func (f *fakeNotifier) Notify(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, text)
	return nil
}

func (f *fakeNotifier) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeNotifier) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

// --- helpers ---

var _ transmission.Interface = (*fakeTrans)(nil)

type testEnv struct {
	store  *fakeStore
	search *fakeSearcher
	trans  *fakeTrans
	notify *fakeNotifier
	engine *Engine
}

func newTestEnv(t *testing.T, subs ...store.Subscription) *testEnv {
	t.Helper()
	env := &testEnv{
		store:  newFakeStore(subs...),
		search: newFakeSearcher(),
		trans:  &fakeTrans{hash: "hash-1"},
		notify: &fakeNotifier{},
	}
	env.engine = NewEngine(env.store, env.search, env.trans, env.notify, time.Hour,
		slog.New(slog.DiscardHandler))
	return env
}

func f1Sub() store.Subscription {
	return store.Subscription{
		ID:      1,
		Query:   "space show 2026",
		Include: []string{"rus", "1080p"},
		Exclude: []string{"720p"},
	}
}

const gb = int64(1) << 30

// --- tick behavior ---

func TestTickGrabsNewMatchingRelease(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g-match", Title: "Космос 2026 Серия 5 [1080p, Rus]", Size: 20 * gb,
			DownloadURL: "http://prowlarr/dl/1", InfoHash: "ih1"},
		{GUID: "g-lowres", Title: "Космос 2026 [720p, Rus]", Size: 4 * gb,
			DownloadURL: "http://prowlarr/dl/2"},
	}
	env.search.torrents["http://prowlarr/dl/1"] = []byte("meta-1")

	env.engine.Tick(context.Background())

	if got := env.trans.torrents; len(got) != 1 || string(got[0]) != "meta-1" {
		t.Fatalf("AddTorrent calls = %q, want exactly [meta-1]", got)
	}
	if !env.store.seen[seenKey(1, "g-match")] {
		t.Error("matching release not marked seen")
	}
	if got := env.store.seenTitles[seenKey(1, "g-match")]; !strings.Contains(got, "Серия 5") {
		t.Errorf("seen title = %q, want the release title recorded with the mark", got)
	}
	if env.store.seen[seenKey(1, "g-lowres")] {
		t.Error("non-matching release marked seen")
	}
	if env.store.grabs[1] != 1 {
		t.Errorf("grabs = %d, want 1", env.store.grabs[1])
	}
	if len(env.store.downloads) != 1 {
		t.Fatalf("downloads = %+v, want 1 row", env.store.downloads)
	}
	dl := env.store.downloads[0]
	if dl.Hash != "hash-1" || dl.Source != "sub:1" || !strings.Contains(dl.Title, "1080p") {
		t.Errorf("download row = %+v, want hash-1 / sub:1 / matching title", dl)
	}
	msgs := env.notify.messages()
	if len(msgs) != 1 {
		t.Fatalf("notifications = %q, want exactly 1", msgs)
	}
	if !strings.Contains(msgs[0], "#1") || !strings.Contains(msgs[0], "Серия 5") {
		t.Errorf("notification %q must contain sub id #1 and release title", msgs[0])
	}
	if env.store.lastChecked[1].IsZero() {
		t.Error("last_checked_at not updated after successful run")
	}
}

func TestTickSkipsSeenReleases(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.store.seen[seenKey(1, "g1")] = true

	env.engine.Tick(context.Background())

	if env.trans.addCount() != 0 {
		t.Error("seen release was added again")
	}
	if len(env.notify.messages()) != 0 {
		t.Error("seen release triggered a notification")
	}
	if env.store.grabs[1] != 0 {
		t.Error("seen release incremented grabs")
	}
	if env.store.lastChecked[1].IsZero() {
		t.Error("last check not recorded for a tick with nothing new")
	}
}

func TestTickUsesMagnetWhenNoTorrentFile(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb,
			MagnetURL: "magnet:?xt=urn:btih:abc"},
	}

	env.engine.Tick(context.Background())

	if got := env.trans.magnets; len(got) != 1 || got[0] != "magnet:?xt=urn:btih:abc" {
		t.Fatalf("AddMagnet calls = %q, want the magnet link", got)
	}
	if !env.store.seen[seenKey(1, "g1")] {
		t.Error("magnet release not marked seen")
	}
}

func TestTickReleaseWithoutAnyLinkIsSkippedUnseen(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb},
	}

	env.engine.Tick(context.Background())

	if env.trans.addCount() != 0 {
		t.Error("linkless release was added")
	}
	if env.store.seenCount() != 0 {
		t.Error("linkless release marked seen")
	}
	if len(env.notify.messages()) != 0 {
		t.Error("linkless release triggered a notification")
	}
}

// --- failure semantics ---

func TestTickProwlarrUnreachableRetriesNextTick(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.search.searchErr["space show 2026"] = errors.New("connection refused")

	env.engine.Tick(context.Background())

	if env.trans.addCount() != 0 || env.store.seenCount() != 0 || len(env.notify.messages()) != 0 {
		t.Fatal("failed search must add nothing, mark nothing seen, notify nothing")
	}
	if !env.store.lastChecked[1].IsZero() {
		t.Error("failed search recorded a last check")
	}

	// Prowlarr comes back: the same release is grabbed on the next tick.
	env.search.mu.Lock()
	delete(env.search.searchErr, "space show 2026")
	env.search.mu.Unlock()
	env.engine.Tick(context.Background())

	if env.trans.addCount() != 1 {
		t.Errorf("adds after recovery = %d, want 1", env.trans.addCount())
	}
	if !env.store.seen[seenKey(1, "g1")] {
		t.Error("release not marked seen after recovery")
	}
}

func TestTickTransmissionFailureLeavesReleaseUnseen(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.trans.addErr = errors.New("transmission down")

	env.engine.Tick(context.Background())

	if env.store.seenCount() != 0 {
		t.Fatal("release marked seen although Transmission add failed")
	}
	if env.store.grabs[1] != 0 || len(env.notify.messages()) != 0 {
		t.Fatal("failed add must not count as a grab or notify")
	}

	// Transmission recovers: next tick grabs exactly once.
	env.trans.mu.Lock()
	env.trans.addErr = nil
	env.trans.mu.Unlock()
	env.engine.Tick(context.Background())

	if env.trans.addCount() != 1 || env.store.grabs[1] != 1 {
		t.Errorf("after recovery adds=%d grabs=%d, want 1/1", env.trans.addCount(), env.store.grabs[1])
	}
}

func TestTickFetchTorrentFailureLeavesReleaseUnseen(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.fetchErr = errors.New("proxy error")

	env.engine.Tick(context.Background())

	if env.trans.addCount() != 0 || env.store.seenCount() != 0 {
		t.Fatal("failed torrent fetch must add nothing and mark nothing seen")
	}
}

func TestTickFetchFailureFallsBackToMagnet(t *testing.T) {
	// Prowlarr answers magnet-backed downloadUrls with a redirect to a
	// magnet: URI the HTTP fetch cannot follow — the magnet link must be used.
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb,
			DownloadURL: "u1", MagnetURL: "magnet:?xt=urn:btih:abc"},
	}
	env.search.fetchErr = errors.New(`unsupported protocol scheme "magnet"`)

	env.engine.Tick(context.Background())

	if got := env.trans.magnets; len(got) != 1 || got[0] != "magnet:?xt=urn:btih:abc" {
		t.Fatalf("AddMagnet calls = %q, want the magnet fallback", got)
	}
	if !env.store.seen[seenKey(1, "g1")] {
		t.Error("magnet-fallback release not marked seen")
	}
}

func TestTickIsSeenErrorSkipsRelease(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.store.isSeenErr = errors.New("database locked")

	env.engine.Tick(context.Background())

	if env.trans.addCount() != 0 || len(env.notify.messages()) != 0 || env.store.grabs[1] != 0 {
		t.Fatal("a failed seen lookup must skip the release, not risk a duplicate grab")
	}

	// The store recovers: the next tick grabs the release normally.
	env.store.mu.Lock()
	env.store.isSeenErr = nil
	env.store.mu.Unlock()
	env.engine.Tick(context.Background())

	if env.trans.addCount() != 1 || env.store.grabs[1] != 1 {
		t.Errorf("after recovery adds=%d grabs=%d, want 1/1", env.trans.addCount(), env.store.grabs[1])
	}
}

func TestTickMarkSeenFailureReAddsNextTickTransmissionDedupes(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.store.markSeenErr = errors.New("disk full")

	env.engine.Tick(context.Background())

	// The add went through and is announced even though seen-bookkeeping failed.
	if env.trans.addCount() != 1 || len(env.notify.messages()) != 1 {
		t.Fatalf("adds=%d notifications=%d, want 1/1 despite MarkSeen failure",
			env.trans.addCount(), len(env.notify.messages()))
	}
	if env.store.seenCount() != 0 {
		t.Fatal("MarkSeen failed but the release ended up seen")
	}

	// Documented worst case: the release is re-added next tick and only
	// Transmission's duplicate handling saves it.
	env.engine.Tick(context.Background())
	if env.trans.addCount() != 2 {
		t.Errorf("adds after second tick = %d, want 2 (re-add, deduped by Transmission)", env.trans.addCount())
	}

	// Once MarkSeen recovers the release settles as seen.
	env.store.mu.Lock()
	env.store.markSeenErr = nil
	env.store.mu.Unlock()
	env.engine.Tick(context.Background())
	if !env.store.seen[seenKey(1, "g1")] {
		t.Error("release not marked seen after MarkSeen recovered")
	}
}

func TestTickAddDownloadFailureStillMarksSeenAndNotifies(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.store.addDLErr = errors.New("disk full")

	env.engine.Tick(context.Background())

	// The torrent is already downloading in Transmission: the release stays
	// seen and the grab is announced; only the completion notification is lost.
	if !env.store.seen[seenKey(1, "g1")] {
		t.Error("release not marked seen after AddDownload failure")
	}
	if env.store.grabs[1] != 1 || len(env.notify.messages()) != 1 {
		t.Errorf("grabs=%d notifications=%d, want 1/1", env.store.grabs[1], len(env.notify.messages()))
	}
	if len(env.store.downloads) != 0 {
		t.Errorf("downloads = %+v, want none recorded", env.store.downloads)
	}

	// Not re-added on the next tick: the seen mark stuck.
	env.engine.Tick(context.Background())
	if env.trans.addCount() != 1 {
		t.Errorf("adds after second tick = %d, want still 1", env.trans.addCount())
	}
}

func TestTickNotifyFailureKeepsReleaseSeen(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.notify.setErr(errors.New("telegram down"))

	env.engine.Tick(context.Background())

	// The grab announcement is lost for good, but the release must stay seen
	// so it is never grabbed twice.
	if !env.store.seen[seenKey(1, "g1")] {
		t.Fatal("release not marked seen after notify failure")
	}
	if env.store.grabs[1] != 1 {
		t.Errorf("grabs = %d, want 1", env.store.grabs[1])
	}

	env.notify.setErr(nil)
	env.engine.Tick(context.Background())
	if env.trans.addCount() != 1 || len(env.notify.messages()) != 0 {
		t.Errorf("adds=%d notifications=%d after recovery, want 1/0 (never re-announced)",
			env.trans.addCount(), len(env.notify.messages()))
	}
}

func TestTickGrabsStatsFailuresAreNonFatal(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")
	env.store.grabsErr = errors.New("db locked")
	env.store.lastCheckedErr = errors.New("db locked")

	env.engine.Tick(context.Background())

	// Stats bookkeeping failures must not block the grab or its announcement.
	if env.trans.addCount() != 1 || len(env.notify.messages()) != 1 {
		t.Errorf("adds=%d notifications=%d, want 1/1", env.trans.addCount(), len(env.notify.messages()))
	}
	if !env.store.seen[seenKey(1, "g1")] {
		t.Error("release not marked seen")
	}
}

func TestTickCapsGrabsPerSubscription(t *testing.T) {
	sub := store.Subscription{ID: 1, Query: "everything"} // no filters: matches all
	env := newTestEnv(t, sub)
	var releases []prowlarr.Release
	for i := 0; i < maxGrabsPerTick+5; i++ {
		url := fmt.Sprintf("u%d", i)
		releases = append(releases, prowlarr.Release{
			GUID: fmt.Sprintf("g%d", i), Title: fmt.Sprintf("Release %d", i),
			Size: gb, DownloadURL: url,
		})
		env.search.torrents[url] = []byte("meta")
	}
	env.search.results["everything"] = releases

	env.engine.Tick(context.Background())

	if got := env.trans.addCount(); got != maxGrabsPerTick {
		t.Fatalf("first tick adds = %d, want capped at %d", got, maxGrabsPerTick)
	}
	if got := len(env.notify.messages()); got != maxGrabsPerTick {
		t.Errorf("first tick notifications = %d, want %d", got, maxGrabsPerTick)
	}

	// The remainder is picked up on the next tick.
	env.engine.Tick(context.Background())
	if got := env.trans.addCount(); got != maxGrabsPerTick+5 {
		t.Fatalf("adds after second tick = %d, want %d", got, maxGrabsPerTick+5)
	}
	if env.store.seenCount() != maxGrabsPerTick+5 {
		t.Errorf("seen = %d, want all %d marked", env.store.seenCount(), maxGrabsPerTick+5)
	}
}

func TestTickTwoSubsSameRelease(t *testing.T) {
	subA := store.Subscription{ID: 1, Query: "space show"}
	subB := store.Subscription{ID: 2, Query: "space opera"}
	env := newTestEnv(t, subA, subB)
	release := prowlarr.Release{GUID: "g-shared", Title: "Космос [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"}
	env.search.results["space show"] = []prowlarr.Release{release}
	env.search.results["space opera"] = []prowlarr.Release{release}
	env.search.torrents["u1"] = []byte("meta")

	env.engine.Tick(context.Background())

	// Both subs grab it independently; Transmission dedupes the actual add,
	// the downloads table keeps one row, and both users' subs count the grab.
	if len(env.store.downloads) != 1 {
		t.Errorf("download rows = %d, want 1 (deduped by hash)", len(env.store.downloads))
	}
	if env.store.grabs[1] != 1 || env.store.grabs[2] != 1 {
		t.Errorf("grabs = %d/%d, want 1/1", env.store.grabs[1], env.store.grabs[2])
	}
	if got := len(env.notify.messages()); got != 2 {
		t.Errorf("notifications = %d, want 2 (one per sub)", got)
	}
	if !env.store.seen[seenKey(1, "g-shared")] || !env.store.seen[seenKey(2, "g-shared")] {
		t.Error("release not marked seen for both subscriptions")
	}
}

func TestTickSkipsPausedSubscriptions(t *testing.T) {
	paused := f1Sub()
	paused.Paused = true
	env := newTestEnv(t, paused)
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}

	env.engine.Tick(context.Background())

	if env.search.searchCount() != 0 {
		t.Error("paused subscription was searched")
	}
	if !env.store.lastChecked[1].IsZero() {
		t.Error("paused subscription recorded a check")
	}
}

func TestTickOneSubFailureDoesNotBlockOthers(t *testing.T) {
	broken := f1Sub()
	healthy := store.Subscription{ID: 2, Query: "ubuntu iso"}
	env := newTestEnv(t, broken, healthy)
	env.search.searchErr["space show 2026"] = errors.New("indexer exploded")
	env.search.results["ubuntu iso"] = []prowlarr.Release{
		{GUID: "g-ubuntu", Title: "Ubuntu 26.04 LTS", Size: 3 * gb, DownloadURL: "u2"},
	}
	env.search.torrents["u2"] = []byte("ubuntu-meta")

	env.engine.Tick(context.Background())

	if got := env.search.searchedQueries(); len(got) != 2 {
		t.Fatalf("searched %q, want both subscriptions", got)
	}
	if !env.store.seen[seenKey(2, "g-ubuntu")] {
		t.Error("healthy subscription not processed after another sub failed")
	}
	if env.store.grabs[2] != 1 {
		t.Errorf("healthy sub grabs = %d, want 1", env.store.grabs[2])
	}
}

func TestTickListSubscriptionsFailureIsQuiet(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.store.listErr = errors.New("database locked")

	env.engine.Tick(context.Background()) // must not panic

	if env.search.searchCount() != 0 {
		t.Error("searched despite list failure")
	}
}

func TestDuplicateReleaseAcrossTicksGrabbedOnce(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.search.results["space show 2026"] = []prowlarr.Release{
		{GUID: "g1", Title: "Космос 2026 [1080p, Rus]", Size: 20 * gb, DownloadURL: "u1"},
	}
	env.search.torrents["u1"] = []byte("meta")

	env.engine.Tick(context.Background())
	env.engine.Tick(context.Background())
	env.engine.Tick(context.Background())

	if env.trans.addCount() != 1 {
		t.Errorf("adds = %d, want exactly 1 across ticks", env.trans.addCount())
	}
	if env.store.grabs[1] != 1 {
		t.Errorf("grabs = %d, want exactly 1 across ticks", env.store.grabs[1])
	}
	if got := env.notify.messages(); len(got) != 1 {
		t.Errorf("notifications = %q, want exactly 1 across ticks", got)
	}
}

// --- constructor + Run: ticker loop ---

func TestNewEngineNonPositiveIntervalFallsBack(t *testing.T) {
	// A zero interval must never reach time.NewTicker (it panics there).
	for _, interval := range []time.Duration{0, -5 * time.Minute} {
		e := NewEngine(newFakeStore(), newFakeSearcher(), &fakeTrans{}, &fakeNotifier{}, interval, nil)
		if e.interval != DefaultTickInterval {
			t.Errorf("NewEngine(interval=%v).interval = %v, want default %v", interval, e.interval, DefaultTickInterval)
		}
		if e.log == nil {
			t.Error("nil logger not replaced with a default")
		}
	}
}

func TestRunFirstTickIsImmediate(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	// Interval is one hour: any search observed must come from the immediate tick.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.engine.Run(ctx)
		close(done)
	}()

	select {
	case <-env.search.searchedC:
	case <-time.After(2 * time.Second):
		t.Fatal("no immediate first tick within 2s")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on context cancel")
	}
}

func TestRunTicksPeriodically(t *testing.T) {
	env := newTestEnv(t, f1Sub())
	env.engine.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		env.engine.Run(ctx)
		close(done)
	}()

	// Immediate tick plus at least two ticker fires.
	for i := 0; i < 3; i++ {
		select {
		case <-env.search.searchedC:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d tick(s) observed within 2s", i)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on context cancel")
	}
}
