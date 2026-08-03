package tgbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

// --- fake subscription store ---

type pauseCall struct {
	id     int64
	paused bool
}

type fakeSubStore struct {
	subs       map[int64]store.Subscription
	created    []store.Subscription
	deleted    []int64
	pauses     []pauseCall
	active     []store.Download
	completed  []store.Download
	recorded   []store.Download // what FindDownloads matches against
	findTitles []string         // every title FindDownloads was asked about
	added      []recordedDownload
	createErr  error
	getErr     error
	listErr    error
	deleteErr  error
	pauseErr   error
	activeErr  error
	findErr    error
	doneErr    error
	addDLErr   error
	cancelled  []string
	cancelErr  error
}

func newFakeSubStore(subs ...store.Subscription) *fakeSubStore {
	m := make(map[int64]store.Subscription, len(subs))
	for _, s := range subs {
		m[s.ID] = s
	}
	return &fakeSubStore{subs: m}
}

func (f *fakeSubStore) CreateSubscription(_ context.Context, sub store.Subscription) (store.Subscription, error) {
	if f.createErr != nil {
		return store.Subscription{}, f.createErr
	}
	sub.ID = int64(len(f.created) + 1)
	f.created = append(f.created, sub)
	f.subs[sub.ID] = sub
	return sub, nil
}

func (f *fakeSubStore) CancelDownload(_ context.Context, hash string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelled = append(f.cancelled, hash)
	return nil
}

func (f *fakeSubStore) GetSubscription(_ context.Context, id int64) (store.Subscription, error) {
	if f.getErr != nil {
		return store.Subscription{}, f.getErr
	}
	sub, ok := f.subs[id]
	if !ok {
		return store.Subscription{}, fmt.Errorf("subscription %d: %w", id, store.ErrNotFound)
	}
	return sub, nil
}

func (f *fakeSubStore) ListSubscriptions(context.Context) ([]store.Subscription, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	subs := make([]store.Subscription, 0, len(f.subs))
	for _, s := range f.subs {
		subs = append(subs, s)
	}
	slices.SortFunc(subs, func(a, b store.Subscription) int { return int(a.ID - b.ID) })
	return subs, nil
}

func (f *fakeSubStore) DeleteSubscription(_ context.Context, id int64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.subs[id]; !ok {
		return fmt.Errorf("subscription %d: %w", id, store.ErrNotFound)
	}
	delete(f.subs, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeSubStore) SetSubscriptionPaused(_ context.Context, id int64, paused bool) error {
	if f.pauseErr != nil {
		return f.pauseErr
	}
	sub, ok := f.subs[id]
	if !ok {
		return fmt.Errorf("subscription %d: %w", id, store.ErrNotFound)
	}
	sub.Paused = paused
	f.subs[id] = sub
	f.pauses = append(f.pauses, pauseCall{id, paused})
	return nil
}

func (f *fakeSubStore) ActiveDownloads(context.Context) ([]store.Download, error) {
	return f.active, f.activeErr
}

// FindDownloads matches recorded downloads the way the store does: hash
// case-insensitively, title exactly.
func (f *fakeSubStore) FindDownloads(_ context.Context, hashes, titles []string) ([]store.Download, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	f.findTitles = append(f.findTitles, titles...)
	var found []store.Download
	for _, dl := range f.recorded {
		byHash := slices.ContainsFunc(hashes, func(h string) bool { return strings.EqualFold(h, dl.Hash) })
		if byHash || slices.Contains(titles, dl.Title) {
			found = append(found, dl)
		}
	}
	return found, nil
}

func (f *fakeSubStore) RecentCompleted(_ context.Context, limit int) ([]store.Download, error) {
	if f.doneErr != nil {
		return nil, f.doneErr
	}
	return f.completed[:min(limit, len(f.completed))], nil
}

func (f *fakeSubStore) AddDownload(_ context.Context, hash, title, source string) error {
	if f.addDLErr != nil {
		return f.addDLErr
	}
	f.added = append(f.added, recordedDownload{hash, title, source})
	return nil
}

var _ SubscriptionStore = (*fakeSubStore)(nil)

// newCommandHandlers wires Handlers with a specific fake subscription store.
func newCommandHandlers(s *fakeSearcher, tr *fakeTrans, subs *fakeSubStore) *Handlers {
	return NewHandlers(s, tr, subs, slog.New(slog.DiscardHandler))
}

// command runs a /command text update through the full HandleText path.
func command(t *testing.T, h *Handlers, tg *fakeTG, text string) {
	t.Helper()
	h.HandleText(context.Background(), tg, textUpdate(text))
}

// --- /sub ---

func TestSubCommandCreatesSubscription(t *testing.T) {
	subs := newFakeSubStore()
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/sub space show 2026 | rus, 1080p, -720p, >1gb")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	got := subs.created[0]
	if got.Query != "space show 2026" {
		t.Errorf("Query = %q, want %q", got.Query, "space show 2026")
	}
	if want := []string{"rus", "1080p"}; !slices.Equal(got.Include, want) {
		t.Errorf("Include = %v, want %v", got.Include, want)
	}
	if want := []string{"720p"}; !slices.Equal(got.Exclude, want) {
		t.Errorf("Exclude = %v, want %v", got.Exclude, want)
	}
	if got.MinSizeMB != 1024 || got.MaxSizeMB != 0 {
		t.Errorf("size bounds = (%d, %d) MB, want (1024, 0)", got.MinSizeMB, got.MaxSizeMB)
	}

	reply := tg.lastSentText(t)
	if !strings.Contains(reply, "#1") || !strings.Contains(reply, "space show 2026") {
		t.Errorf("reply %q missing subscription id or query", reply)
	}
	if !strings.Contains(reply, "rus") || !strings.Contains(reply, ">1gb") {
		t.Errorf("reply %q should echo the parsed filters", reply)
	}
}

func TestSubCommandRegexFilterKeepsPipe(t *testing.T) {
	subs := newFakeSubStore()
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	// The filter part contains "|" inside a regex; only the FIRST "|" splits.
	command(t, h, tg, "/sub dune | rus, -/cam|ts/")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	got := subs.created[0]
	if got.Query != "dune" {
		t.Errorf("Query = %q, want dune", got.Query)
	}
	if want := []string{"/cam|ts/"}; !slices.Equal(got.Exclude, want) {
		t.Errorf("Exclude = %v, want %v", got.Exclude, want)
	}
}

func TestSubCommandNoFilters(t *testing.T) {
	subs := newFakeSubStore()
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/sub dune part three")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	got := subs.created[0]
	if got.Query != "dune part three" {
		t.Errorf("Query = %q, want %q", got.Query, "dune part three")
	}
	if len(got.Include) != 0 || len(got.Exclude) != 0 || got.MinSizeMB != 0 || got.MaxSizeMB != 0 {
		t.Errorf("filterless /sub produced non-empty filter: %+v", got)
	}
}

func TestSubCommandEmptyQuery(t *testing.T) {
	for _, text := range []string{"/sub", "/sub   ", "/sub | rus, 1080p"} {
		t.Run(text, func(t *testing.T) {
			subs := newFakeSubStore()
			h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
			tg := &fakeTG{}

			command(t, h, tg, text)

			if len(subs.created) != 0 {
				t.Error("empty query must not create a subscription")
			}
			if got := tg.lastSentText(t); !strings.Contains(got, "Usage") {
				t.Errorf("reply %q should show usage", got)
			}
		})
	}
}

func TestSubCommandBadFilter(t *testing.T) {
	subs := newFakeSubStore()
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/sub dune | /ba[d/, >notasize")

	if len(subs.created) != 0 {
		t.Error("bad filter must not create a subscription")
	}
	got := tg.lastSentText(t)
	if !strings.Contains(got, "filter") {
		t.Errorf("reply %q should explain the filter is bad", got)
	}
}

func TestSubCommandStoreError(t *testing.T) {
	subs := newFakeSubStore()
	subs.createErr = errors.New("disk full")
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/sub dune")

	if got := tg.lastSentText(t); !strings.Contains(got, "disk full") {
		t.Errorf("reply %q should surface the store error", got)
	}
}

func TestSubCommandNewlineAfterCommand(t *testing.T) {
	// A plausible phone input: newline instead of a space after the command.
	subs := newFakeSubStore()
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/sub\nspace show | rus")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	if got := subs.created[0].Query; got != "space show" {
		t.Errorf("Query = %q, want %q", got, "space show")
	}
	if want := []string{"rus"}; !slices.Equal(subs.created[0].Include, want) {
		t.Errorf("Include = %v, want %v", subs.created[0].Include, want)
	}
}

// --- /subs ---

func TestSubsCommandListsAll(t *testing.T) {
	subs := newFakeSubStore(
		store.Subscription{
			ID:            1,
			Query:         "space show 2026",
			Include:       []string{"rus", "1080p"},
			Exclude:       []string{"720p"},
			MinSizeMB:     1024,
			Grabs:         3,
			LastCheckedAt: time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		},
		store.Subscription{ID: 2, Query: "dune", Paused: true},
	)
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/subs")

	got := tg.lastSentText(t)
	for _, want := range []string{
		"#1", "space show 2026",
		"rus, 1080p, -720p, >1gb", // filter string round-trip
		"grabs: 3",
		"2026-07-31 10:00", // last check
		"#2", "dune",
		"paused",
		"never", // sub 2 has never been checked
	} {
		if !strings.Contains(got, want) {
			t.Errorf("subscription list %q missing %q", got, want)
		}
	}
	if strings.Count(got, "paused") != 1 {
		t.Errorf("list %q should mark only the paused subscription", got)
	}
}

func TestSubsCommandEmpty(t *testing.T) {
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, newFakeSubStore())
	tg := &fakeTG{}

	command(t, h, tg, "/subs")

	if got := tg.lastSentText(t); !strings.Contains(got, "No subscriptions") {
		t.Errorf("reply %q should say there are no subscriptions", got)
	}
}

func TestSubsCommandStoreError(t *testing.T) {
	subs := newFakeSubStore()
	subs.listErr = errors.New("db locked")
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/subs")

	if got := tg.lastSentText(t); !strings.Contains(got, "db locked") {
		t.Errorf("reply %q should surface the store error", got)
	}
}

// --- /unsub ---

func TestUnsubCommandDeletes(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 2, Query: "dune"})
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/unsub 2")

	if !slices.Equal(subs.deleted, []int64{2}) {
		t.Errorf("deleted = %v, want [2]", subs.deleted)
	}
	if got := tg.lastSentText(t); !strings.Contains(got, "#2") {
		t.Errorf("reply %q should confirm removal of #2", got)
	}
}

func TestUnsubCommandUnknownID(t *testing.T) {
	subs := newFakeSubStore()
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/unsub 99")

	if got := tg.lastSentText(t); !strings.Contains(got, "not found") {
		t.Errorf("reply %q should say the subscription was not found", got)
	}
}

func TestUnsubCommandBadID(t *testing.T) {
	for _, text := range []string{"/unsub", "/unsub abc", "/unsub -1"} {
		t.Run(text, func(t *testing.T) {
			subs := newFakeSubStore(store.Subscription{ID: 1, Query: "q"})
			h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
			tg := &fakeTG{}

			command(t, h, tg, text)

			if len(subs.deleted) != 0 {
				t.Error("bad id must not delete anything")
			}
			if got := tg.lastSentText(t); !strings.Contains(got, "Usage") {
				t.Errorf("reply %q should show usage", got)
			}
		})
	}
}

func TestUnsubCommandGenericStoreError(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 1, Query: "q"})
	subs.deleteErr = errors.New("database locked")
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/unsub 1")

	got := tg.lastSentText(t)
	if !strings.Contains(got, "database locked") {
		t.Errorf("reply %q should surface the store error", got)
	}
	if strings.Contains(got, "not found") {
		t.Errorf("reply %q must not claim the subscription was not found", got)
	}
}

// --- /pause ---

func TestPauseCommandTogglesPause(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 3, Query: "q"})
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/pause 3")

	if want := []pauseCall{{3, true}}; !slices.Equal(subs.pauses, want) {
		t.Fatalf("pause calls = %v, want %v", subs.pauses, want)
	}
	if got := tg.lastSentText(t); !strings.Contains(strings.ToLower(got), "paused") {
		t.Errorf("reply %q should say the subscription is paused", got)
	}

	// Second /pause on the (now paused) subscription resumes it.
	command(t, h, tg, "/pause 3")

	if want := []pauseCall{{3, true}, {3, false}}; !slices.Equal(subs.pauses, want) {
		t.Fatalf("pause calls = %v, want %v", subs.pauses, want)
	}
	if got := tg.lastSentText(t); !strings.Contains(strings.ToLower(got), "resumed") {
		t.Errorf("reply %q should say the subscription is resumed", got)
	}
}

func TestPauseCommandUnknownID(t *testing.T) {
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, newFakeSubStore())
	tg := &fakeTG{}

	command(t, h, tg, "/pause 7")

	if got := tg.lastSentText(t); !strings.Contains(got, "not found") {
		t.Errorf("reply %q should say the subscription was not found", got)
	}
}

func TestPauseCommandGenericStoreError(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 3, Query: "q"})
	subs.pauseErr = errors.New("database locked")
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/pause 3")

	got := tg.lastSentText(t)
	if !strings.Contains(got, "database locked") {
		t.Errorf("reply %q should surface the store error", got)
	}
	if strings.Contains(strings.ToLower(got), "resumed") || strings.Contains(got, "⏸ Paused") {
		t.Errorf("reply %q must not claim the toggle succeeded", got)
	}
}

// --- /test (dry run) ---

func TestTestCommandAcksBeforeSearching(t *testing.T) {
	// A dry run hits the same slow Prowlarr search as a plain query, so it
	// gets the same "I'm on it" ack — plain, not edited: the dry-run answer
	// is a list and may be chunked across several messages.
	subs := newFakeSubStore(store.Subscription{ID: 1, Query: "космос"})
	s := &fakeSearcher{releases: []prowlarr.Release{
		{GUID: "a", Title: "Космос [1080p]", Size: 4 << 30, Seeders: 12},
	}}
	h := newCommandHandlers(s, &fakeTrans{}, subs)
	tg := &fakeTG{}

	var sentWhileSearching int
	s.onSearch = func() {
		tg.mu.Lock()
		defer tg.mu.Unlock()
		sentWhileSearching = len(tg.sent)
	}

	command(t, h, tg, "/test 1")

	if sentWhileSearching != 1 {
		t.Fatalf("sent %d messages while searching, want 1 ack", sentWhileSearching)
	}
	if !strings.Contains(tg.sent[0].Text, "космос") {
		t.Errorf("ack %q does not name the query", tg.sent[0].Text)
	}
	if len(tg.edited) != 0 {
		t.Errorf("edited %d messages, want 0 (the dry-run answer is sent fresh)", len(tg.edited))
	}
	if got := tg.lastSentText(t); !strings.Contains(got, "Dry run") {
		t.Errorf("last message %q should be the dry-run result", got)
	}
}

func TestTestCommandGenericStoreError(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 1, Query: "q"})
	subs.getErr = errors.New("database locked")
	s := &fakeSearcher{}
	h := newCommandHandlers(s, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/test 1")

	if len(s.queries) != 0 {
		t.Error("a failed subscription load must not trigger a search")
	}
	got := tg.lastSentText(t)
	if !strings.Contains(got, "database locked") {
		t.Errorf("reply %q should surface the store error", got)
	}
	if strings.Contains(got, "not found") {
		t.Errorf("reply %q must not claim the subscription was not found", got)
	}
}

func TestTestCommandDryRun(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{
		ID:      5,
		Query:   "космос",
		Include: []string{"rus", "1080p"},
		Exclude: []string{"720p"},
	})
	s := &fakeSearcher{releases: []prowlarr.Release{
		{GUID: "a", Title: "Космос [1080p, x265, Rus]", Size: 4 << 30, Seeders: 120},
		{GUID: "b", Title: "Space Show [720p, Rus]", Size: 2 << 30, Seeders: 300},
		{GUID: "c", Title: "Space Show [1080p, Eng]", Size: 5 << 30, Seeders: 90},
	}}
	tr := &fakeTrans{}
	h := newCommandHandlers(s, tr, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/test 5")

	if got := s.queries; len(got) != 1 || got[0] != "космос" {
		t.Fatalf("searched %v, want the subscription query", got)
	}

	got := tg.lastSentText(t)
	if !strings.Contains(got, "Космос [1080p, x265, Rus]") {
		t.Errorf("dry-run reply %q missing the matching release", got)
	}
	if strings.Contains(got, "720p, Rus]") || strings.Contains(got, "Eng]") {
		t.Errorf("dry-run reply %q includes non-matching releases", got)
	}
	if !strings.Contains(got, "1 of 3") {
		t.Errorf("dry-run reply %q should report exactly \"1 of 3\" matches", got)
	}

	// Dry run: nothing downloaded, nothing recorded, seen-table untouched
	// (the command handlers have no access to seen-table methods at all).
	if len(s.fetched) != 0 || len(tr.meta) != 0 || len(tr.magnets) != 0 {
		t.Error("dry run must not fetch or add anything")
	}
	if len(subs.added) != 0 {
		t.Error("dry run must not record downloads")
	}
}

func TestTestCommandNoMatches(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 1, Query: "q", Include: []string{"zzz"}})
	s := &fakeSearcher{releases: nReleases(3)}
	h := newCommandHandlers(s, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/test 1")

	if got := tg.lastSentText(t); !strings.Contains(got, "0 of 3") {
		t.Errorf("reply %q should report 0 of 3 matches", got)
	}
}

func TestTestCommandCapsLongLists(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 1, Query: "q"}) // empty filter matches all
	s := &fakeSearcher{releases: nReleases(12)}
	h := newCommandHandlers(s, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/test 1")

	got := tg.lastSentText(t)
	if strings.Count(got, "• ") != maxDryRunLines {
		t.Errorf("reply lists %d releases, want capped at %d", strings.Count(got, "• "), maxDryRunLines)
	}
	if !strings.Contains(got, "2 more") {
		t.Errorf("reply %q should mention the 2 releases over the cap", got)
	}
}

func TestTestCommandUnknownID(t *testing.T) {
	s := &fakeSearcher{}
	h := newCommandHandlers(s, &fakeTrans{}, newFakeSubStore())
	tg := &fakeTG{}

	command(t, h, tg, "/test 9")

	if len(s.queries) != 0 {
		t.Error("unknown id must not trigger a search")
	}
	if got := tg.lastSentText(t); !strings.Contains(got, "not found") {
		t.Errorf("reply %q should say the subscription was not found", got)
	}
}

func TestTestCommandSearchError(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 1, Query: "q"})
	s := &fakeSearcher{searchErr: errors.New(`indexer "TrackerB" failed`)}
	h := newCommandHandlers(s, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/test 1")

	if got := tg.lastSentText(t); !strings.Contains(got, "TrackerB") {
		t.Errorf("reply %q should surface the search error", got)
	}
}

// --- /status ---

func TestStatusCommandRendersProgress(t *testing.T) {
	subs := newFakeSubStore()
	subs.active = []store.Download{
		{ID: 1, Hash: "aaa", Title: "Ubuntu ISO", Status: store.StatusActive},
		{ID: 2, Hash: "bbb", Title: "Old Movie", Status: store.StatusActive},
		{ID: 3, Hash: "ccc", Title: "Done Thing", Status: store.StatusActive},
	}
	tr := &fakeTrans{statuses: []transmission.TorrentStatus{
		{Name: "ubuntu.iso", Hash: "aaa", Percent: 0.42, Rate: 5 << 20, ETA: 12 * time.Minute},
		{Name: "done.mkv", Hash: "ccc", Percent: 1, Done: true},
		// "bbb" is missing: removed from Transmission externally.
	}}
	h := newCommandHandlers(&fakeSearcher{}, tr, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	got := tg.lastSentText(t)
	for _, want := range []string{
		"Ubuntu ISO",
		"▰▰▰▰▱▱▱▱▱▱", // 42% → 4 of 10 cells
		"42%",
		"5MB/s",
		"12m",
		"Old Movie",
		"not in Transmission",
		"Done Thing",
		"100%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status reply %q missing %q", got, want)
		}
	}
}

func TestStatusCommandEmpty(t *testing.T) {
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, newFakeSubStore())
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	if got := tg.lastSentText(t); !strings.Contains(got, "No downloads yet") {
		t.Errorf("reply %q should say there are no downloads at all", got)
	}
}

func TestStatusCommandListsCompleted(t *testing.T) {
	added := time.Date(2026, 7, 30, 9, 11, 0, 0, time.UTC)
	subs := newFakeSubStore()
	subs.active = []store.Download{{ID: 3, Hash: "aaa", Title: "Running", Status: store.StatusActive}}
	subs.completed = []store.Download{
		{ID: 2, Hash: "bbb", Title: "Finished Later", Status: store.StatusDone, AddedAt: added},
		{ID: 1, Hash: "ccc", Title: "Finished Earlier", Status: store.StatusDone, AddedAt: added},
	}
	tr := &fakeTrans{statuses: []transmission.TorrentStatus{{Hash: "aaa", Percent: 0.5}}}
	h := newCommandHandlers(&fakeSearcher{}, tr, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	got := tg.lastSentText(t)
	for _, want := range []string{
		"⬇️ Active downloads", "Running",
		"✅ Recently completed", "Finished Later", "Finished Earlier",
		// The store records when a download was added, not when it finished.
		"added 2026-07-30 09:11",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status reply %q missing %q", got, want)
		}
	}
}

func TestStatusCommandOmitsEmptySections(t *testing.T) {
	subs := newFakeSubStore()
	subs.completed = []store.Download{{ID: 1, Hash: "bbb", Title: "Only History", Status: store.StatusDone}}
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	got := tg.lastSentText(t)
	if !strings.Contains(got, "Only History") {
		t.Errorf("reply %q should list the completed download", got)
	}
	if strings.Contains(got, "Active downloads") {
		t.Errorf("reply %q should omit the empty active section", got)
	}
}

func TestStatusCommandCapsCompleted(t *testing.T) {
	subs := newFakeSubStore()
	for i := range maxCompletedLines + 5 {
		subs.completed = append(subs.completed, store.Download{
			ID:     int64(i),
			Hash:   fmt.Sprintf("h%d", i),
			Title:  fmt.Sprintf("Done %d", i),
			Status: store.StatusDone,
		})
	}
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	if got := strings.Count(tg.lastSentText(t), "\n• "); got != maxCompletedLines {
		t.Errorf("status listed %d completed downloads, want %d", got, maxCompletedLines)
	}
}

func TestStatusCommandCompletedStoreError(t *testing.T) {
	subs := newFakeSubStore()
	subs.doneErr = errors.New("database locked")
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	got := tg.lastSentText(t)
	if !strings.Contains(got, "Failed to list downloads") || !strings.Contains(got, "database locked") {
		t.Errorf("reply %q should surface the store error", got)
	}
}

func TestStatusCommandStoreError(t *testing.T) {
	subs := newFakeSubStore()
	subs.activeErr = errors.New("database locked")
	tr := &fakeTrans{}
	h := newCommandHandlers(&fakeSearcher{}, tr, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	got := tg.lastSentText(t)
	if !strings.Contains(got, "Failed to list downloads") || !strings.Contains(got, "database locked") {
		t.Errorf("reply %q should surface the store error", got)
	}
}

func TestStatusCommandTransmissionError(t *testing.T) {
	subs := newFakeSubStore()
	subs.active = []store.Download{{ID: 1, Hash: "aaa", Title: "Still Running", Status: store.StatusActive}}
	subs.completed = []store.Download{{ID: 2, Hash: "bbb", Title: "From History", Status: store.StatusDone}}
	tr := &fakeTrans{activeErr: errors.New("connection refused")}
	h := newCommandHandlers(&fakeSearcher{}, tr, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	got := tg.lastSentText(t)
	if !strings.Contains(got, "connection refused") {
		t.Errorf("reply %q should surface the Transmission error", got)
	}
	// Only the live progress figures need Transmission. Both lists come from
	// the store, so an outage must not hide them.
	for _, want := range []string{"Still Running", "From History"} {
		if !strings.Contains(got, want) {
			t.Errorf("reply %q lost %q to the Transmission outage", got, want)
		}
	}
}

func TestStatusCommandHashMatchingIsCaseInsensitive(t *testing.T) {
	// Same normalization as the completion watcher: Transmission's hash casing
	// is not guaranteed to match what was stored.
	subs := newFakeSubStore()
	subs.active = []store.Download{
		{ID: 1, Hash: "ABCDEF", Title: "Case Mismatch", Status: store.StatusActive},
	}
	tr := &fakeTrans{statuses: []transmission.TorrentStatus{
		{Name: "case.mkv", Hash: "abcdef", Percent: 0.5, Rate: 1 << 20, ETA: time.Minute},
	}}
	h := newCommandHandlers(&fakeSearcher{}, tr, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/status")

	got := tg.lastSentText(t)
	if strings.Contains(got, "not in Transmission") {
		t.Errorf("status reply %q wrongly reports a live download as removed", got)
	}
	if !strings.Contains(got, "50%") {
		t.Errorf("status reply %q missing the 50%% progress", got)
	}
}

func TestSubsCommandLongListIsChunked(t *testing.T) {
	// Enough long subscriptions to blow past Telegram's 4096-char limit: the
	// reply must arrive as several messages, each within the limit.
	var list []store.Subscription
	for i := 1; i <= 60; i++ {
		list = append(list, store.Subscription{
			ID:    int64(i),
			Query: fmt.Sprintf("Космос сезон 2026 этап %d полная гонка запись %s", i, strings.Repeat("x", 80)),
		})
	}
	subs := newFakeSubStore(list...)
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/subs")

	if len(tg.sent) < 2 {
		t.Fatalf("sent %d messages, want the long list split into several", len(tg.sent))
	}
	for i, msg := range tg.sent {
		if n := len([]rune(msg.Text)); n > maxMessageLen {
			t.Errorf("chunk %d is %d runes, over the %d limit", i, n, maxMessageLen)
		}
	}
	all := ""
	for _, msg := range tg.sent {
		all += msg.Text + "\n"
	}
	if !strings.Contains(all, "#1 ") && !strings.Contains(all, "#1 «") {
		t.Error("first subscription missing from the chunked output")
	}
	if !strings.Contains(all, "#60") {
		t.Error("last subscription missing from the chunked output")
	}
}

// --- /help, command routing, menu ---

func TestHelpCommandListsAllCommands(t *testing.T) {
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, newFakeSubStore())
	tg := &fakeTG{}

	command(t, h, tg, "/help")

	got := tg.lastSentText(t)
	for _, cmd := range []string{"/sub", "/subs", "/unsub", "/pause", "/test", "/status", "/help"} {
		if !strings.Contains(got, cmd) {
			t.Errorf("help text missing %s", cmd)
		}
	}
}

func TestCommandWithBotNameSuffix(t *testing.T) {
	subs := newFakeSubStore(store.Subscription{ID: 1, Query: "q"})
	h := newCommandHandlers(&fakeSearcher{}, &fakeTrans{}, subs)
	tg := &fakeTG{}

	command(t, h, tg, "/subs@torrent_bot")

	if got := tg.lastSentText(t); !strings.Contains(got, "#1") {
		t.Errorf("reply %q should list subscriptions for /subs@botname", got)
	}
}

func TestCommandMenuCoversAllCommands(t *testing.T) {
	menu := commandMenu()
	want := []string{"sub", "subs", "unsub", "pause", "test", "status", "help"}
	if len(menu) != len(want) {
		t.Fatalf("menu has %d entries, want %d", len(menu), len(want))
	}
	for i, cmd := range want {
		if menu[i].Command != cmd {
			t.Errorf("menu[%d].Command = %q, want %q", i, menu[i].Command, cmd)
		}
		if menu[i].Description == "" {
			t.Errorf("menu entry %q has no description", cmd)
		}
	}
}

// --- rendering helpers ---

func TestProgressBar(t *testing.T) {
	tests := []struct {
		percent float64
		want    string
	}{
		{0, "▱▱▱▱▱▱▱▱▱▱"},
		{0.05, "▱▱▱▱▱▱▱▱▱▱"},
		{0.42, "▰▰▰▰▱▱▱▱▱▱"},
		{0.5, "▰▰▰▰▰▱▱▱▱▱"},
		{1, "▰▰▰▰▰▰▰▰▰▰"},
		{1.5, "▰▰▰▰▰▰▰▰▰▰"}, // clamped
		{-1, "▱▱▱▱▱▱▱▱▱▱"},  // clamped
	}
	for _, tt := range tests {
		if got := progressBar(tt.percent); got != tt.want {
			t.Errorf("progressBar(%v) = %q, want %q", tt.percent, got, tt.want)
		}
	}
}

func TestHumanETA(t *testing.T) {
	tests := []struct {
		eta  time.Duration
		want string
	}{
		{-time.Second, "—"},
		{45 * time.Second, "45s"},
		{12 * time.Minute, "12m"},
		{90 * time.Minute, "1h30m"},
	}
	for _, tt := range tests {
		if got := humanETA(tt.eta); got != tt.want {
			t.Errorf("humanETA(%v) = %q, want %q", tt.eta, got, tt.want)
		}
	}
}

// --- subscription cutoff ---

func TestCmdSubDefaultsToFutureOnly(t *testing.T) {
	h, subs := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	before := time.Now().UTC()
	h.handleCommand(context.Background(), tg, testChatID, "/sub space show 2026 | rus, 1080p")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	got := subs.created[0]
	if got.CutoffAt.Before(before) {
		t.Errorf("cutoff = %v, want roughly now (%v)", got.CutoffAt, before)
	}
	if !slices.Equal(got.Include, []string{"rus", "1080p"}) {
		t.Errorf("include = %v, want [rus 1080p]", got.Include)
	}
	if text := tg.lastSentText(t); !strings.Contains(text, "onward") {
		t.Errorf("confirmation %q must state the cutoff", text)
	}
}

// backlog is a subscription setting, not a title pattern: leaving it in the
// filter list would turn it into a required substring matching nothing.
func TestCmdSubBacklogTokenClearsCutoffAndIsNotAFilter(t *testing.T) {
	h, subs := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	h.handleCommand(context.Background(), tg, testChatID, "/sub the wire | 1080p, backlog, -720p")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	got := subs.created[0]
	if !got.CutoffAt.IsZero() {
		t.Errorf("cutoff = %v, want zero with the backlog token", got.CutoffAt)
	}
	if !slices.Equal(got.Include, []string{"1080p"}) {
		t.Errorf("include = %v, want [1080p] — backlog must not become a pattern", got.Include)
	}
	if !slices.Equal(got.Exclude, []string{"720p"}) {
		t.Errorf("exclude = %v, want [720p]", got.Exclude)
	}
	if text := tg.lastSentText(t); !strings.Contains(text, "including older releases") {
		t.Errorf("confirmation %q must state that the backlog is included", text)
	}
}

func TestCmdSubBacklogTokenIsCaseInsensitive(t *testing.T) {
	h, subs := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	h.handleCommand(context.Background(), tg, testChatID, "/sub the wire | BackLog")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	if !subs.created[0].CutoffAt.IsZero() {
		t.Errorf("cutoff = %v, want zero", subs.created[0].CutoffAt)
	}
	if len(subs.created[0].Include) != 0 {
		t.Errorf("include = %v, want none", subs.created[0].Include)
	}
}

// A release whose title happens to contain the word is a different thing from
// the token standing on its own.
func TestCmdSubBacklogInsideAPatternStaysAFilter(t *testing.T) {
	h, subs := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	h.handleCommand(context.Background(), tg, testChatID, "/sub docs | backlog show")

	if len(subs.created) != 1 {
		t.Fatalf("created %d subscriptions, want 1", len(subs.created))
	}
	got := subs.created[0]
	if !slices.Equal(got.Include, []string{"backlog show"}) {
		t.Errorf("include = %v, want [backlog show]", got.Include)
	}
	if got.CutoffAt.IsZero() {
		t.Error("cutoff = zero, want the default cutoff — the token was part of a pattern")
	}
}

func TestSubsListShowsCutoff(t *testing.T) {
	cutoff := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	h.subs = newFakeSubStore(
		store.Subscription{ID: 1, Query: "space show", CutoffAt: cutoff},
		store.Subscription{ID: 2, Query: "the wire"},
	)
	tg := &fakeTG{}

	h.handleCommand(context.Background(), tg, testChatID, "/subs")

	text := tg.lastSentText(t)
	if !strings.Contains(text, "from: 2026-08-03") {
		t.Errorf("/subs output must show the cutoff date:\n%s", text)
	}
	if !strings.Contains(text, "backlog included") {
		t.Errorf("/subs output must show that a cutoff-less subscription takes the backlog:\n%s", text)
	}
}

// The dry run and the engine must agree about what a subscription will grab,
// or /test stops being worth running.
func TestCmdTestAppliesTheCutoff(t *testing.T) {
	cutoff := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	s := &fakeSearcher{releases: []prowlarr.Release{
		{GUID: "old", Title: "Космос 2026 Серия 10 [1080p]", Size: 2 << 30,
			PublishDate: cutoff.Add(-24 * time.Hour)},
		{GUID: "new", Title: "Космос 2026 Серия 11 [1080p]", Size: 2 << 30,
			PublishDate: cutoff.Add(24 * time.Hour)},
	}}
	h, _ := newTestHandlers(s, &fakeTrans{})
	h.subs = newFakeSubStore(store.Subscription{ID: 1, Query: "space show", CutoffAt: cutoff})
	tg := &fakeTG{}

	h.handleCommand(context.Background(), tg, testChatID, "/test 1")

	text := tg.lastSentText(t)
	if !strings.Contains(text, "1 of 2 results match") {
		t.Errorf("dry run must count only what the engine would grab:\n%s", text)
	}
	if strings.Contains(text, "Серия 10") {
		t.Errorf("dry run listed a release older than the cutoff:\n%s", text)
	}
}
