package tgbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/transmission"
)

const testChatID = int64(42)

// --- fakes ---

type fakeTG struct {
	mu        sync.Mutex
	sent      []*bot.SendMessageParams
	edited    []*bot.EditMessageTextParams
	answered  []*bot.AnswerCallbackQueryParams
	sendCalls int     // every SendMessage attempt, including failed ones
	sendErr   error   // fails every SendMessage
	sendErrs  []error // per-call overrides, consumed in order; nil = success
	editErr   error
}

func (f *fakeTG) SendMessage(_ context.Context, p *bot.SendMessageParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls++
	err := f.sendErr
	if len(f.sendErrs) > 0 {
		err = f.sendErrs[0]
		f.sendErrs = f.sendErrs[1:]
	}
	if err != nil {
		return nil, err
	}
	f.sent = append(f.sent, p)
	return &models.Message{ID: len(f.sent)}, nil
}

func (f *fakeTG) EditMessageText(_ context.Context, p *bot.EditMessageTextParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.edited = append(f.edited, p)
	return &models.Message{ID: p.MessageID}, nil
}

func (f *fakeTG) AnswerCallbackQuery(_ context.Context, p *bot.AnswerCallbackQueryParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, p)
	return true, nil
}

func (f *fakeTG) lastSentText(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no messages sent")
	}
	return f.sent[len(f.sent)-1].Text
}

func (f *fakeTG) lastEditedText(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edited) == 0 {
		t.Fatal("no messages edited")
	}
	return f.edited[len(f.edited)-1].Text
}

type fakeSearcher struct {
	releases  []prowlarr.Release
	searchErr error
	torrent   []byte
	fetchErr  error
	queries   []string
	fetched   []string
	// onSearch runs at the start of Search, so tests can observe what the
	// chat looked like while the (slow) search was still in flight.
	onSearch func()
}

func (f *fakeSearcher) Search(_ context.Context, query string) ([]prowlarr.Release, error) {
	if f.onSearch != nil {
		f.onSearch()
	}
	f.queries = append(f.queries, query)
	return f.releases, f.searchErr
}

func (f *fakeSearcher) FetchTorrent(_ context.Context, url string) ([]byte, error) {
	f.fetched = append(f.fetched, url)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.torrent, nil
}

type fakeTrans struct {
	hash      string
	addErr    error
	meta      [][]byte
	magnets   []string
	statuses  []transmission.TorrentStatus
	activeErr error
}

func (f *fakeTrans) AddTorrent(_ context.Context, meta []byte) (string, error) {
	f.meta = append(f.meta, meta)
	return f.hash, f.addErr
}

func (f *fakeTrans) AddMagnet(_ context.Context, link string) (string, error) {
	f.magnets = append(f.magnets, link)
	return f.hash, f.addErr
}

func (f *fakeTrans) Active(context.Context) ([]transmission.TorrentStatus, error) {
	return f.statuses, f.activeErr
}

type recordedDownload struct{ hash, title, source string }

// --- helpers ---

// newTestHandlers wires Handlers over a fresh fake store, returning both so
// tests can assert on recorded downloads.
func newTestHandlers(s *fakeSearcher, tr *fakeTrans) (*Handlers, *fakeSubStore) {
	subs := newFakeSubStore()
	return NewHandlers(testChatID, s, tr, subs, slog.New(slog.DiscardHandler)), subs
}

func textUpdate(text string) *models.Update {
	return &models.Update{Message: &models.Message{
		Text: text,
		Chat: models.Chat{ID: testChatID},
	}}
}

func callbackUpdate(data string) *models.Update {
	return &models.Update{CallbackQuery: &models.CallbackQuery{
		ID:   "cb1",
		Data: data,
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: 7, Chat: models.Chat{ID: testChatID}},
		},
	}}
}

func nReleases(n int) []prowlarr.Release {
	releases := make([]prowlarr.Release, n)
	for i := range releases {
		releases[i] = prowlarr.Release{
			GUID:        fmt.Sprintf("guid-%d", i),
			Title:       fmt.Sprintf("Release %d", i),
			Size:        int64(i+1) << 30,
			Seeders:     1000 - i,
			DownloadURL: fmt.Sprintf("http://prowlarr/dl/%d", i),
		}
	}
	return releases
}

func keyboardOf(t *testing.T, markup models.ReplyMarkup) *models.InlineKeyboardMarkup {
	t.Helper()
	kb, ok := markup.(*models.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("reply markup is %T, want *models.InlineKeyboardMarkup", markup)
	}
	return kb
}

// --- search flow ---

func TestHandleTextAcksBeforeSearching(t *testing.T) {
	// A Prowlarr search can take minutes, so the chat must show that the bot
	// picked the query up *before* the search runs, not after.
	s := &fakeSearcher{releases: nReleases(3)}
	h, _ := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{}

	var ack *bot.SendMessageParams
	s.onSearch = func() {
		tg.mu.Lock()
		defer tg.mu.Unlock()
		if len(tg.sent) != 1 {
			t.Errorf("sent %d messages while searching, want 1 ack", len(tg.sent))
			return
		}
		ack = tg.sent[0]
	}

	h.HandleText(context.Background(), tg, textUpdate("space show"))

	if ack == nil {
		t.Fatal("no ack was sent before the search started")
	}
	if ack.ChatID != testChatID {
		t.Errorf("ack ChatID = %v, want %d", ack.ChatID, testChatID)
	}
	if !strings.Contains(ack.Text, "space show") {
		t.Errorf("ack %q does not name the query", ack.Text)
	}
	if ack.ReplyMarkup != nil {
		t.Errorf("ack carries a %T reply markup, want none", ack.ReplyMarkup)
	}
}

func TestHandleTextBuildsKeyboard(t *testing.T) {
	s := &fakeSearcher{releases: nReleases(2*perPage + 5)}
	h, _ := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("space show"))

	if got := s.queries; len(got) != 1 || got[0] != "space show" {
		t.Fatalf("searched queries = %v, want [space show]", got)
	}
	if len(tg.sent) != 1 {
		t.Fatalf("sent %d messages, want 1 (the ack)", len(tg.sent))
	}
	if len(tg.edited) != 1 {
		t.Fatalf("edited %d messages, want 1 (the ack turned into results)", len(tg.edited))
	}
	msg := tg.edited[0]
	if msg.ChatID != testChatID {
		t.Errorf("ChatID = %v, want %d", msg.ChatID, testChatID)
	}
	if msg.MessageID != 1 {
		t.Errorf("edited message %d, want 1 (the ack message)", msg.MessageID)
	}
	if !strings.Contains(msg.Text, "space show") || !strings.Contains(msg.Text, "1/3") {
		t.Errorf("header %q missing query or page 1/3", msg.Text)
	}

	kb := keyboardOf(t, msg.ReplyMarkup)
	if len(kb.InlineKeyboard) != perPage+1 { // 10 results + nav row
		t.Fatalf("keyboard has %d rows, want %d", len(kb.InlineKeyboard), perPage+1)
	}

	kind, id, n, err := decodeCallback(kb.InlineKeyboard[0][0].CallbackData)
	if err != nil || kind != cbDownload || n != 0 {
		t.Errorf("first button data = (%q, %q, %d, %v), want dl:<id>:0",
			kind, id, n, err)
	}
	if _, ok := h.cache.Get(id); !ok {
		t.Error("search id from button is not in the cache")
	}

	nav := kb.InlineKeyboard[perPage]
	if len(nav) != 1 {
		t.Fatalf("nav row has %d buttons, want 1 (next only on first page)", len(nav))
	}
	kind, _, n, err = decodeCallback(nav[0].CallbackData)
	if err != nil || kind != cbPage || n != 1 {
		t.Errorf("nav button = (%q, %d, %v), want pg -> page 1", kind, n, err)
	}
}

// --- download-state markers ---

func TestSearchMarksAlreadyGrabbedReleases(t *testing.T) {
	releases := nReleases(3)
	releases[0].InfoHash = "AABBCC" // matched by hash, in a different casing
	releases[1].InfoHash = ""       // no hash published: matched by title

	s := &fakeSearcher{releases: releases}
	h, subs := newTestHandlers(s, &fakeTrans{})
	subs.recorded = []store.Download{
		{ID: 1, Hash: "aabbcc", Title: "Something Else Entirely", Status: store.StatusActive},
		{ID: 2, Hash: "ddeeff", Title: releases[1].Title, Status: store.StatusDone},
	}
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("space show"))

	msg := tg.edited[0]
	if !strings.Contains(msg.Text, "1. "+markActive) {
		t.Errorf("result 1 should be marked as downloading:\n%s", msg.Text)
	}
	if !strings.Contains(msg.Text, "2. "+markDone) {
		t.Errorf("result 2 should be marked as downloaded:\n%s", msg.Text)
	}
	if strings.Contains(msg.Text, "3. "+markActive) || strings.Contains(msg.Text, "3. "+markDone) {
		t.Errorf("result 3 was never grabbed and must carry no marker:\n%s", msg.Text)
	}

	// The marker reaches the buttons too — that is where the tap happens.
	kb := keyboardOf(t, msg.ReplyMarkup)
	if !strings.Contains(kb.InlineKeyboard[0][0].Text, markActive) {
		t.Errorf("button 1 = %q, want the downloading marker", kb.InlineKeyboard[0][0].Text)
	}
	if !strings.Contains(kb.InlineKeyboard[1][0].Text, markDone) {
		t.Errorf("button 2 = %q, want the downloaded marker", kb.InlineKeyboard[1][0].Text)
	}
}

func TestSearchLooksUpOnlyThePagesReleases(t *testing.T) {
	// The lookup must stay bounded by the page, not by the size of the result
	// set — a search can return hundreds of releases.
	s := &fakeSearcher{releases: nReleases(3 * perPage)}
	h, subs := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("space show"))

	if len(subs.findTitles) != perPage {
		t.Errorf("looked up %d titles, want %d (one page)", len(subs.findTitles), perPage)
	}
}

func TestPageFlipRecomputesMarks(t *testing.T) {
	// Marks are not cached with the search: a page flip re-renders the whole
	// message, and a torrent that finished meanwhile must show its new state.
	releases := nReleases(perPage + 2)
	s := &fakeSearcher{releases: releases}
	h, subs := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("space show"))
	_, id, _, err := decodeCallback(keyboardOf(t, tg.edited[0].ReplyMarkup).InlineKeyboard[0][0].CallbackData)
	if err != nil {
		t.Fatalf("decodeCallback = %v", err)
	}

	// Only a release on page 2 is recorded, and only after the first render.
	subs.recorded = []store.Download{
		{ID: 1, Hash: "zzz", Title: releases[perPage].Title, Status: store.StatusDone},
	}
	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbPage, id, 1)))

	flipped := tg.edited[len(tg.edited)-1]
	if !strings.Contains(flipped.Text, fmt.Sprintf("%d. %s", perPage+1, markDone)) {
		t.Errorf("page 2 should mark the finished release:\n%s", flipped.Text)
	}
	if !strings.Contains(keyboardOf(t, flipped.ReplyMarkup).InlineKeyboard[0][0].Text, markDone) {
		t.Error("page 2's first button lost the marker")
	}
}

func TestSearchSurvivesMarkLookupFailure(t *testing.T) {
	// A search can cost minutes; losing the markers must never cost the answer.
	s := &fakeSearcher{releases: nReleases(3)}
	h, subs := newTestHandlers(s, &fakeTrans{})
	subs.findErr = errors.New("database locked")
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("space show"))

	if len(tg.edited) != 1 {
		t.Fatalf("edited %d messages, want 1 (the results)", len(tg.edited))
	}
	msg := tg.edited[0]
	if !strings.Contains(msg.Text, "1. Release 0") {
		t.Errorf("results should render unmarked:\n%s", msg.Text)
	}
	if strings.Contains(msg.Text, markActive) || strings.Contains(msg.Text, markDone) {
		t.Errorf("a failed lookup must produce no markers:\n%s", msg.Text)
	}
	if n := len(keyboardOf(t, msg.ReplyMarkup).InlineKeyboard); n != 3 { // 3 results, no nav row
		t.Errorf("keyboard has %d rows, want 3", n)
	}
}

func TestHandleTextSearchErrorSurfacesIndexer(t *testing.T) {
	s := &fakeSearcher{searchErr: errors.New(`indexer "TrackerA" failed: timeout`)}
	h, _ := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("dune"))

	if got := tg.lastEditedText(t); !strings.Contains(got, "TrackerA") {
		t.Errorf("reply %q does not surface the failing indexer", got)
	}
}

func TestHandleTextNoResults(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("nothing here"))

	got := tg.lastEditedText(t)
	if !strings.Contains(got, "No results") || !strings.Contains(got, "nothing here") {
		t.Errorf("reply %q should say there are no results for the query", got)
	}
}

func TestHandleTextAckSendFailureStillDelivers(t *testing.T) {
	// The ack is a courtesy: losing it must not cost the user their results.
	s := &fakeSearcher{releases: nReleases(2*perPage + 5)}
	h, _ := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{sendErrs: []error{errors.New("telegram hiccup")}}

	h.HandleText(context.Background(), tg, textUpdate("space show"))

	if len(s.queries) != 1 {
		t.Fatalf("searched %d times, want 1 despite the failed ack", len(s.queries))
	}
	if len(tg.edited) != 0 {
		t.Errorf("edited %d messages, want 0 (there is no ack to edit)", len(tg.edited))
	}
	if len(tg.sent) != 1 {
		t.Fatalf("delivered %d messages, want 1 fresh results message", len(tg.sent))
	}
	msg := tg.sent[0]
	if !strings.Contains(msg.Text, "1/3") {
		t.Errorf("results header %q missing page 1/3", msg.Text)
	}
	if keyboardOf(t, msg.ReplyMarkup) == nil {
		t.Error("fresh results message has no keyboard")
	}
}

func TestHandleTextAckEditFailureFallsBackToFreshMessage(t *testing.T) {
	s := &fakeSearcher{releases: nReleases(2*perPage + 5)}
	h, _ := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{editErr: errors.New("message to edit not found")}

	h.HandleText(context.Background(), tg, textUpdate("space show"))

	if len(tg.sent) != 2 {
		t.Fatalf("sent %d messages, want 2 (ack + fresh results after the edit failed)", len(tg.sent))
	}
	msg := tg.sent[1]
	if !strings.Contains(msg.Text, "1/3") {
		t.Errorf("results header %q missing page 1/3", msg.Text)
	}
	if keyboardOf(t, msg.ReplyMarkup) == nil {
		t.Error("fresh results message has no keyboard")
	}
}

func TestHandleTextIgnoresEmptyAndNonMessage(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("   "))
	h.HandleText(context.Background(), tg, &models.Update{})

	if len(tg.sent) != 0 {
		t.Errorf("sent %d messages, want 0", len(tg.sent))
	}
}

func TestHandleTextUnknownCommand(t *testing.T) {
	s := &fakeSearcher{}
	h, _ := newTestHandlers(s, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleText(context.Background(), tg, textUpdate("/bogus"))

	if len(s.queries) != 0 {
		t.Error("a /command should not trigger a search")
	}
	if got := tg.lastSentText(t); !strings.Contains(got, "/help") {
		t.Errorf("reply %q should point at /help", got)
	}
}

// --- download callback ---

func TestDownloadCallbackFetchesAndAdds(t *testing.T) {
	s := &fakeSearcher{torrent: []byte("d4:infoe")}
	tr := &fakeTrans{hash: "abc123"}
	h, subs := newTestHandlers(s, tr)
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: []prowlarr.Release{{
		Title:       "Космос [1080p, Rus]",
		DownloadURL: "http://prowlarr/dl/1",
	}}})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbDownload, id, 0)))

	if len(tg.answered) != 1 {
		t.Errorf("answered %d callback queries, want 1", len(tg.answered))
	}
	if got := s.fetched; len(got) != 1 || got[0] != "http://prowlarr/dl/1" {
		t.Errorf("fetched = %v, want the release downloadUrl", got)
	}
	if len(tr.meta) != 1 || string(tr.meta[0]) != "d4:infoe" {
		t.Errorf("AddTorrent meta = %v, want fetched torrent bytes", tr.meta)
	}
	if len(tr.magnets) != 0 {
		t.Errorf("AddMagnet called %d times, want 0", len(tr.magnets))
	}
	want := recordedDownload{hash: "abc123", title: "Космос [1080p, Rus]", source: "search"}
	if len(subs.added) != 1 || subs.added[0] != want {
		t.Errorf("recorded downloads = %+v, want [%+v]", subs.added, want)
	}
	if got := tg.lastSentText(t); !strings.Contains(got, "Космос") {
		t.Errorf("confirmation %q missing title", got)
	}
}

func TestDownloadCallbackMagnetOnly(t *testing.T) {
	tr := &fakeTrans{hash: "def456"}
	s := &fakeSearcher{}
	h, _ := newTestHandlers(s, tr)
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: []prowlarr.Release{{
		Title:     "Magnet Release",
		MagnetURL: "magnet:?xt=urn:btih:def456",
	}}})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbDownload, id, 0)))

	if len(s.fetched) != 0 {
		t.Error("FetchTorrent called for a magnet-only release")
	}
	if got := tr.magnets; len(got) != 1 || got[0] != "magnet:?xt=urn:btih:def456" {
		t.Errorf("AddMagnet links = %v, want the magnet URL", got)
	}
}

func TestDownloadCallbackExpiredSearch(t *testing.T) {
	tr := &fakeTrans{}
	h, _ := newTestHandlers(&fakeSearcher{}, tr)
	tg := &fakeTG{}

	h.HandleCallback(context.Background(), tg, callbackUpdate("dl:deadbeef:0"))

	if len(tg.answered) != 1 {
		t.Errorf("answered %d callback queries, want 1", len(tg.answered))
	}
	got := tg.lastSentText(t)
	if !strings.Contains(got, "expired") || !strings.Contains(got, "again") {
		t.Errorf("reply %q should ask the user to search again", got)
	}
	if len(tr.meta) != 0 || len(tr.magnets) != 0 {
		t.Error("nothing should be added for an expired search")
	}
}

func TestDownloadCallbackFetchError(t *testing.T) {
	s := &fakeSearcher{fetchErr: errors.New("boom")}
	tr := &fakeTrans{}
	h, subs := newTestHandlers(s, tr)
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: []prowlarr.Release{{
		Title: "T", DownloadURL: "http://prowlarr/dl/1",
	}}})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbDownload, id, 0)))

	if got := tg.lastSentText(t); !strings.Contains(got, "Failed") {
		t.Errorf("reply %q should report the failure", got)
	}
	if len(tr.meta) != 0 || len(subs.added) != 0 {
		t.Error("failed fetch must not add or record anything")
	}
}

func TestDownloadCallbackAddError(t *testing.T) {
	s := &fakeSearcher{torrent: []byte("meta")}
	tr := &fakeTrans{addErr: errors.New("transmission down")}
	h, subs := newTestHandlers(s, tr)
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: []prowlarr.Release{{
		Title: "T", DownloadURL: "http://prowlarr/dl/1",
	}}})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbDownload, id, 0)))

	if got := tg.lastSentText(t); !strings.Contains(got, "Failed") {
		t.Errorf("reply %q should report the failure", got)
	}
	if len(subs.added) != 0 {
		t.Error("failed add must not be recorded as a download")
	}
}

func TestDownloadCallbackRecorderFailureStillConfirms(t *testing.T) {
	// The torrent IS downloading in Transmission; a failed bookkeeping write
	// only loses the completion notification and must not alarm the user.
	s := &fakeSearcher{torrent: []byte("meta")}
	tr := &fakeTrans{hash: "abc123"}
	h, subs := newTestHandlers(s, tr)
	subs.addDLErr = errors.New("disk full")
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: []prowlarr.Release{{
		Title: "T", DownloadURL: "http://prowlarr/dl/1",
	}}})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbDownload, id, 0)))

	if len(tr.meta) != 1 {
		t.Fatalf("AddTorrent calls = %d, want 1", len(tr.meta))
	}
	got := tg.lastSentText(t)
	if !strings.Contains(got, "Added") {
		t.Errorf("reply %q should still confirm the add", got)
	}
	if strings.Contains(got, "disk full") {
		t.Errorf("reply %q should not surface the bookkeeping error", got)
	}
}

func TestDownloadCallbackNoLink(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: []prowlarr.Release{{Title: "T"}}})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbDownload, id, 0)))

	if got := tg.lastSentText(t); !strings.Contains(got, "Failed") {
		t.Errorf("reply %q should report the missing link", got)
	}
}

func TestDownloadCallbackIndexOutOfRange(t *testing.T) {
	tr := &fakeTrans{}
	h, _ := newTestHandlers(&fakeSearcher{}, tr)
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: []prowlarr.Release{{Title: "T"}}})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbDownload, id, 5)))

	if len(tr.meta) != 0 || len(tr.magnets) != 0 {
		t.Error("out-of-range index must not add anything")
	}
	if len(tg.sent) != 1 {
		t.Fatalf("sent %d messages, want 1 friendly reply", len(tg.sent))
	}
}

// --- pagination callback ---

func TestPageCallbackEditsKeyboard(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: nReleases(2*perPage + 5)})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbPage, id, 1)))

	if len(tg.answered) != 1 {
		t.Errorf("answered %d callback queries, want 1", len(tg.answered))
	}
	if len(tg.edited) != 1 {
		t.Fatalf("edited %d messages, want 1", len(tg.edited))
	}
	edit := tg.edited[0]
	if edit.MessageID != 7 {
		t.Errorf("edited message %d, want 7 (the keyboard message)", edit.MessageID)
	}
	if !strings.Contains(edit.Text, "2/3") {
		t.Errorf("edited header %q missing page 2/3", edit.Text)
	}

	kb := keyboardOf(t, edit.ReplyMarkup)
	_, _, n, err := decodeCallback(kb.InlineKeyboard[0][0].CallbackData)
	if err != nil || n != perPage {
		t.Errorf("first button on page 2 has index %d (%v), want %d", n, err, perPage)
	}
	nav := kb.InlineKeyboard[len(kb.InlineKeyboard)-1]
	if len(nav) != 2 {
		t.Errorf("middle page nav row has %d buttons, want prev+next", len(nav))
	}
}

func TestPageCallbackExpired(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	h.HandleCallback(context.Background(), tg, callbackUpdate("pg:deadbeef:1"))

	if len(tg.edited) != 0 {
		t.Error("expired page flip must not edit anything")
	}
	if got := tg.lastSentText(t); !strings.Contains(got, "expired") {
		t.Errorf("reply %q should say the search expired", got)
	}
}

func TestPageCallbackEditFailureFallsBackToFreshMessage(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{editErr: errors.New("message to edit not found")}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: nReleases(2*perPage + 5)})

	h.HandleCallback(context.Background(), tg, callbackUpdate(encodeCallback(cbPage, id, 1)))

	if len(tg.sent) != 1 {
		t.Fatalf("sent %d messages, want 1 fresh page after the edit failed", len(tg.sent))
	}
	msg := tg.sent[0]
	if !strings.Contains(msg.Text, "2/3") {
		t.Errorf("fresh page header %q missing page 2/3", msg.Text)
	}
	if keyboardOf(t, msg.ReplyMarkup) == nil {
		t.Error("fresh page has no keyboard")
	}
}

func TestPageCallbackInaccessibleMessageSendsFresh(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: nReleases(2*perPage + 5)})
	update := &models.Update{CallbackQuery: &models.CallbackQuery{
		ID:   "cb1",
		Data: encodeCallback(cbPage, id, 2),
		Message: models.MaybeInaccessibleMessage{
			Type:                models.MaybeInaccessibleMessageTypeInaccessibleMessage,
			InaccessibleMessage: &models.InaccessibleMessage{Chat: models.Chat{ID: testChatID}},
		},
	}}

	h.HandleCallback(context.Background(), tg, update)

	if len(tg.edited) != 0 {
		t.Error("edited an inaccessible message")
	}
	if len(tg.sent) != 1 || !strings.Contains(tg.sent[0].Text, "3/3") {
		t.Fatalf("sent = %d messages, want 1 fresh page 3/3", len(tg.sent))
	}
}

// --- malformed callback data ---

func TestHandleCallbackMalformedData(t *testing.T) {
	for _, data := range []string{"garbage", "dl:abc:x", "dl", "::", ""} {
		t.Run(data, func(t *testing.T) {
			tr := &fakeTrans{}
			h, subs := newTestHandlers(&fakeSearcher{}, tr)
			tg := &fakeTG{}

			h.HandleCallback(context.Background(), tg, callbackUpdate(data)) // must not panic

			if len(tg.answered) != 1 {
				t.Errorf("answered %d callback queries, want 1", len(tg.answered))
			}
			if len(tg.sent) != 0 {
				t.Errorf("sent %d messages for malformed data, want 0", len(tg.sent))
			}
			if len(tr.meta) != 0 || len(tr.magnets) != 0 || len(subs.added) != 0 {
				t.Error("malformed callback data must not add or record anything")
			}
		})
	}
}

func TestHandleCallbackUnknownKind(t *testing.T) {
	tr := &fakeTrans{}
	h, subs := newTestHandlers(&fakeSearcher{}, tr)
	tg := &fakeTG{}

	id := h.cache.Put(cachedSearch{Query: "q", Releases: nReleases(3)})

	h.HandleCallback(context.Background(), tg, callbackUpdate("xx:"+id+":0"))

	if len(tg.sent) != 0 || len(tg.edited) != 0 {
		t.Error("unknown callback kind must not reply or edit")
	}
	if len(tr.meta) != 0 || len(tr.magnets) != 0 || len(subs.added) != 0 {
		t.Error("unknown callback kind must not add or record anything")
	}
}

// --- reply chunking ---

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  []string
	}{
		{"short text passes through", "hello", 10, []string{"hello"}},
		{"exactly at limit", "12345", 5, []string{"12345"}},
		{"splits at newline", "aaa\nbbb\nccc", 7, []string{"aaa", "bbb\nccc"}},
		{"hard-splits a long single line", "abcdefghij", 4, []string{"abcd", "efgh", "ij"}},
		{"cyrillic counted in runes not bytes", "ааа\nббб", 4, []string{"ааа", "ббб"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitMessage(tt.text, tt.limit)
			if len(got) != len(tt.want) {
				t.Fatalf("splitMessage(%q, %d) = %q, want %q", tt.text, tt.limit, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("chunk[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReplyAbortsAfterSendFailure(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	tg := &fakeTG{sendErr: errors.New("telegram down")}

	// Three chunks' worth of text: once the first send fails, the remaining
	// chunks must not be attempted.
	text := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", maxMessageLen-1)+"\n", 3), "\n")
	h.reply(context.Background(), tg, text)

	if tg.sendCalls != 1 {
		t.Errorf("send attempts = %d, want 1 (abort after the first failure)", tg.sendCalls)
	}
	if len(tg.sent) != 0 {
		t.Errorf("delivered %d messages, want 0", len(tg.sent))
	}
}

// --- auth middleware ---

func TestAllowChatMiddleware(t *testing.T) {
	tests := []struct {
		name   string
		update *models.Update
		want   bool
	}{
		{"allowed message", textUpdate("hi"), true},
		{"foreign message", &models.Update{Message: &models.Message{
			Text: "hi", Chat: models.Chat{ID: 666},
		}}, false},
		{"allowed callback", callbackUpdate("dl:x:0"), true},
		{"edited message dropped (no handler consumes them)", &models.Update{EditedMessage: &models.Message{
			Text: "hi", Chat: models.Chat{ID: testChatID},
		}}, false},
		{"foreign callback", &models.Update{CallbackQuery: &models.CallbackQuery{
			ID: "cb", Data: "dl:x:0",
			Message: models.MaybeInaccessibleMessage{
				Type:    models.MaybeInaccessibleMessageTypeMessage,
				Message: &models.Message{ID: 1, Chat: models.Chat{ID: 666}},
			},
		}}, false},
		{"inaccessible-message callback from allowed chat", &models.Update{CallbackQuery: &models.CallbackQuery{
			ID: "cb", Data: "dl:x:0",
			Message: models.MaybeInaccessibleMessage{
				Type:                models.MaybeInaccessibleMessageTypeInaccessibleMessage,
				InaccessibleMessage: &models.InaccessibleMessage{Chat: models.Chat{ID: testChatID}},
			},
		}}, true},
		{"update without chat", &models.Update{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			next := func(context.Context, *bot.Bot, *models.Update) { called = true }
			allowChat(testChatID)(next)(context.Background(), nil, tt.update)
			if called != tt.want {
				t.Errorf("handler called = %v, want %v", called, tt.want)
			}
		})
	}
}

// --- bot construction ---

func TestNewBotOffline(t *testing.T) {
	h, _ := newTestHandlers(&fakeSearcher{}, &fakeTrans{})
	b, err := New("123:testtoken", testChatID, h, bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("New returned nil bot")
	}
}
