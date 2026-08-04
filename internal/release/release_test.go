package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	tgtorrentbot "github.com/freemanoid/tg-torrent-bot"
)

// --- fakes ---

type fakeStore struct {
	meta    map[string]string
	fresh   bool
	getErr  error
	setErr  error
	setCall int
}

func newFakeStore() *fakeStore { return &fakeStore{meta: map[string]string{}} }

func (f *fakeStore) Meta(_ context.Context, key string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.meta[key], nil
}

func (f *fakeStore) SetMeta(_ context.Context, key, value string) error {
	f.setCall++
	if f.setErr != nil {
		return f.setErr
	}
	f.meta[key] = value
	return nil
}

func (f *fakeStore) FreshDatabase() bool { return f.fresh }

type fakeNotifier struct {
	messages []string
	err      error
}

func (f *fakeNotifier) Notify(_ context.Context, text string) error {
	f.messages = append(f.messages, text)
	return f.err
}

// newAnnouncer builds an announcer with an explicit version and changelog so
// tests do not depend on the build-time injected Version.
func newAnnouncer(st Store, n Notifier, version, changelog string) *Announcer {
	a := NewAnnouncer(st, n, nil)
	a.version = version
	a.changelog = changelog
	return a
}

const testChangelog = `# Changelog

## v1.4.0

- Announces its own updates.
- A second thing.

## v1.3.0

- Something older.
`

// rangeChangelog has enough entries — and a two-digit minor — to pin the
// version ordering an upgrade that skips releases depends on.
const rangeChangelog = `# Changelog

## v1.12.0

- Twelve.

## v1.11.0

- Eleven.

## v1.9.0

- Nine.

## v1.2.0

- Two.
`

// --- Announce ---

func TestAnnounceSendsOnceAfterUpgrade(t *testing.T) {
	st := newFakeStore()
	st.meta[announcedKey] = "v1.3.0"
	n := &fakeNotifier{}
	a := newAnnouncer(st, n, "v1.4.0", testChangelog)
	ctx := context.Background()

	if err := a.Announce(ctx); err != nil {
		t.Fatalf("Announce = %v", err)
	}
	if len(n.messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(n.messages))
	}
	if !strings.Contains(n.messages[0], "v1.4.0") || !strings.Contains(n.messages[0], "Announces its own updates.") {
		t.Errorf("message = %q, want the version and its changelog entry", n.messages[0])
	}
	if st.meta[announcedKey] != "v1.4.0" {
		t.Errorf("recorded version = %q, want v1.4.0", st.meta[announcedKey])
	}

	// a second start on the same version stays quiet
	if err := a.Announce(ctx); err != nil {
		t.Fatalf("second Announce = %v", err)
	}
	if len(n.messages) != 1 {
		t.Errorf("sent %d messages after restart, want 1", len(n.messages))
	}
}

func TestAnnounceStaysQuietOnFreshInstall(t *testing.T) {
	st := newFakeStore()
	st.fresh = true
	n := &fakeNotifier{}
	a := newAnnouncer(st, n, "v1.4.0", testChangelog)

	if err := a.Announce(context.Background()); err != nil {
		t.Fatalf("Announce = %v", err)
	}
	if len(n.messages) != 0 {
		t.Errorf("sent %v on a fresh install, want nothing", n.messages)
	}
	if st.meta[announcedKey] != "v1.4.0" {
		t.Errorf("recorded version = %q, want v1.4.0 so the next update announces", st.meta[announcedKey])
	}
}

// Umbrel offers one update at a time but a user who skipped a few gets them
// all in a single container restart, so the announcement has to cover every
// version crossed — not just the one that happens to be running now.
func TestAnnounceCoversEveryVersionSkippedOver(t *testing.T) {
	st := newFakeStore()
	st.meta[announcedKey] = "v1.9.0"
	n := &fakeNotifier{}
	a := newAnnouncer(st, n, "v1.12.0", rangeChangelog)

	if err := a.Announce(context.Background()); err != nil {
		t.Fatalf("Announce = %v", err)
	}
	if len(n.messages) != 1 {
		t.Fatalf("sent %d messages, want 1 covering both versions", len(n.messages))
	}
	for _, want := range []string{"Twelve.", "Eleven."} {
		if !strings.Contains(n.messages[0], want) {
			t.Errorf("message = %q, want it to include %q", n.messages[0], want)
		}
	}
	if strings.Contains(n.messages[0], "Nine.") {
		t.Errorf("message = %q, want nothing from the version already announced", n.messages[0])
	}
}

// A database that predates this feature has no announced version but is not a
// fresh install: the user did upgrade and should hear about it — about this
// version, not about every release ever made.
func TestAnnounceSendsOnFirstUpgradeFromOlderBuild(t *testing.T) {
	st := newFakeStore()
	st.fresh = false
	n := &fakeNotifier{}
	a := newAnnouncer(st, n, "v1.12.0", rangeChangelog)

	if err := a.Announce(context.Background()); err != nil {
		t.Fatalf("Announce = %v", err)
	}
	if len(n.messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(n.messages))
	}
	if !strings.Contains(n.messages[0], "Twelve.") {
		t.Errorf("message = %q, want the current version's notes", n.messages[0])
	}
	if strings.Contains(n.messages[0], "Eleven.") || strings.Contains(n.messages[0], "Two.") {
		t.Errorf("message = %q, want no backlog of older entries", n.messages[0])
	}
}

func TestAnnounceSkipsUnversionedBuild(t *testing.T) {
	for _, version := range []string{DevVersion, "", "   "} {
		st := newFakeStore()
		n := &fakeNotifier{}
		a := newAnnouncer(st, n, version, testChangelog)

		if err := a.Announce(context.Background()); err != nil {
			t.Fatalf("Announce(%q) = %v", version, err)
		}
		if len(n.messages) != 0 || st.setCall != 0 {
			t.Errorf("Announce(%q) sent %v and wrote meta %d times, want neither", version, n.messages, st.setCall)
		}
	}
}

// A send that fails must not be recorded, so the announcement is retried on
// the next start instead of being lost.
func TestAnnounceRetriesAfterSendFailure(t *testing.T) {
	st := newFakeStore()
	st.meta[announcedKey] = "v1.3.0"
	n := &fakeNotifier{err: errors.New("telegram down")}
	a := newAnnouncer(st, n, "v1.4.0", testChangelog)
	ctx := context.Background()

	if err := a.Announce(ctx); err == nil {
		t.Fatal("Announce = nil error, want the send failure")
	}
	if st.meta[announcedKey] != "v1.3.0" {
		t.Fatalf("recorded version = %q, want the old one kept for a retry", st.meta[announcedKey])
	}

	n.err = nil
	if err := a.Announce(ctx); err != nil {
		t.Fatalf("retry Announce = %v", err)
	}
	if st.meta[announcedKey] != "v1.4.0" {
		t.Errorf("recorded version after retry = %q, want v1.4.0", st.meta[announcedKey])
	}
}

func TestAnnounceReportsStoreFailures(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		st := newFakeStore()
		st.getErr = errors.New("boom")
		n := &fakeNotifier{}
		a := newAnnouncer(st, n, "v1.4.0", testChangelog)

		if err := a.Announce(context.Background()); err == nil {
			t.Error("Announce = nil error, want the read failure")
		}
		if len(n.messages) != 0 {
			t.Errorf("sent %v despite an unreadable store, want nothing", n.messages)
		}
	})

	t.Run("write", func(t *testing.T) {
		st := newFakeStore()
		st.meta[announcedKey] = "v1.3.0"
		st.setErr = errors.New("boom")
		n := &fakeNotifier{}
		a := newAnnouncer(st, n, "v1.4.0", testChangelog)

		if err := a.Announce(context.Background()); err == nil {
			t.Error("Announce = nil error, want the write failure")
		}
		if len(n.messages) != 1 {
			t.Errorf("sent %d messages, want the announcement to go out before the failed write", len(n.messages))
		}
	})
}

func TestAnnounceSendsHeadlineWhenChangelogHasNoEntry(t *testing.T) {
	st := newFakeStore()
	n := &fakeNotifier{}
	a := newAnnouncer(st, n, "v9.9.9", testChangelog)

	if err := a.Announce(context.Background()); err != nil {
		t.Fatalf("Announce = %v", err)
	}
	if len(n.messages) != 1 || !strings.Contains(n.messages[0], "v9.9.9") {
		t.Fatalf("messages = %v, want one naming v9.9.9", n.messages)
	}
	if strings.Contains(n.messages[0], "What's new") {
		t.Errorf("message = %q, want no empty changelog section", n.messages[0])
	}
}

// --- Notes ---

func TestNotes(t *testing.T) {
	changelog := `# Changelog

Some preamble.

## v1.4.0

- First item that wraps
  onto a second line.
- Second item.

## v1.3.0 — 2026-08-02

* Older item.
`
	tests := []struct {
		name    string
		version string
		want    []string
	}{
		{"entry with wrapped item", "v1.4.0", []string{"First item that wraps onto a second line.", "Second item."}},
		{"bare version matches v-prefixed heading", "1.4.0", []string{"First item that wraps onto a second line.", "Second item."}},
		{"heading with a trailing date", "v1.3.0", []string{"Older item."}},
		{"unknown version", "v2.0.0", nil},
		{"empty version", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Notes(changelog, tt.version)
			if len(got) != len(tt.want) {
				t.Fatalf("Notes(%q) = %q, want %q", tt.version, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Notes(%q)[%d] = %q, want %q", tt.version, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNotesStopsAtTheNextEntry(t *testing.T) {
	got := Notes(testChangelog, "v1.4.0")
	for _, note := range got {
		if strings.Contains(note, "older") {
			t.Fatalf("Notes leaked the next entry: %q", got)
		}
	}
	if len(got) != 2 {
		t.Errorf("Notes = %q, want the two items of v1.4.0", got)
	}
}

func TestNotesIgnoresBracketedHeadings(t *testing.T) {
	changelog := "## [1.4.0] - 2026-08-02\n\n- Item.\n"
	got := Notes(changelog, "v1.4.0")
	if len(got) != 1 || got[0] != "Item." {
		t.Errorf("Notes = %q, want [\"Item.\"]", got)
	}
}

// The changelog that actually ships must describe the version that ships with
// it, or the announcement would go out with a bare headline.
func TestEmbeddedChangelogCoversItsOwnNewestEntry(t *testing.T) {
	var newest string
	for _, line := range strings.Split(tgtorrentbot.Changelog, "\n") {
		if heading, ok := entryHeading(strings.TrimSpace(line)); ok && heading != "" {
			newest = heading
			break
		}
	}
	if newest == "" {
		t.Fatal("CHANGELOG.md has no `## v<version>` entry")
	}
	if notes := Notes(tgtorrentbot.Changelog, newest); len(notes) == 0 {
		t.Errorf("newest entry %s has no list items", newest)
	}
}

// --- NotesSince ---

func TestNotesSince(t *testing.T) {
	tests := []struct {
		name         string
		from, to     string
		wantVersions []string
		wantNotes    []string // first note of each returned entry
	}{
		{
			name: "every version skipped over is included, newest first",
			from: "v1.2.0", to: "v1.12.0",
			wantVersions: []string{"v1.12.0", "v1.11.0", "v1.9.0"},
			wantNotes:    []string{"Twelve.", "Eleven.", "Nine."},
		},
		{
			name: "a single-step upgrade yields one entry",
			from: "v1.11.0", to: "v1.12.0",
			wantVersions: []string{"v1.12.0"},
			wantNotes:    []string{"Twelve."},
		},
		{
			// Versions are numbers, not strings: "1.9.0" < "1.11.0".
			name: "a two-digit minor sorts above a one-digit one",
			from: "v1.9.0", to: "v1.11.0",
			wantVersions: []string{"v1.11.0"},
			wantNotes:    []string{"Eleven."},
		},
		{
			// A database that predates the announcement feature: announcing
			// every entry ever written would be a wall of text.
			name: "no recorded version yields the current entry alone",
			from: "", to: "v1.12.0",
			wantVersions: []string{"v1.12.0"},
			wantNotes:    []string{"Twelve."},
		},
		{
			name: "a rollback yields the current entry alone",
			from: "v1.12.0", to: "v1.9.0",
			wantVersions: []string{"v1.9.0"},
			wantNotes:    []string{"Nine."},
		},
		{
			name: "an unparseable recorded version yields the current entry alone",
			from: "nightly", to: "v1.12.0",
			wantVersions: []string{"v1.12.0"},
			wantNotes:    []string{"Twelve."},
		},
		{
			name: "a recorded version missing from the changelog still bounds the range",
			from: "v1.10.0", to: "v1.12.0",
			wantVersions: []string{"v1.12.0", "v1.11.0"},
			wantNotes:    []string{"Twelve.", "Eleven."},
		},
		{
			name: "a version with no entry of its own yields nothing to say",
			from: "v1.12.0", to: "v1.13.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NotesSince(rangeChangelog, tt.from, tt.to)
			if len(got) != len(tt.wantVersions) {
				t.Fatalf("NotesSince(%q, %q) = %v, want versions %q", tt.from, tt.to, got, tt.wantVersions)
			}
			for i, e := range got {
				if e.Version != tt.wantVersions[i] {
					t.Errorf("entry %d version = %q, want %q", i, e.Version, tt.wantVersions[i])
				}
				if len(e.Notes) == 0 || e.Notes[0] != tt.wantNotes[i] {
					t.Errorf("entry %d notes = %q, want them to start with %q", i, e.Notes, tt.wantNotes[i])
				}
			}
		})
	}
}

// --- Message ---

func TestMessage(t *testing.T) {
	got := Message("v1.4.0", []Entry{{Version: "v1.4.0", Notes: []string{"One.", "Two."}}})
	want := "🚀 Updated to v1.4.0\n\nWhat's new:\n• One.\n• Two."
	if got != want {
		t.Errorf("Message = %q, want %q", got, want)
	}
}

// Skipping releases must show which notes belong to which version, or the
// whole list reads as one release's worth of news.
func TestMessageLabelsEachVersionWhenSeveralAreAnnounced(t *testing.T) {
	got := Message("v1.12.0", []Entry{
		{Version: "v1.12.0", Notes: []string{"Twelve."}},
		{Version: "v1.11.0", Notes: []string{"Eleven."}},
	})
	want := "🚀 Updated to v1.12.0\n\nWhat's new:\n\nv1.12.0\n• Twelve.\n\nv1.11.0\n• Eleven."
	if got != want {
		t.Errorf("Message = %q, want %q", got, want)
	}
}

// The version that shipped without a changelog entry of its own must not have
// an older entry's notes read as its own.
func TestMessageLabelsAnEntryThatIsNotTheCurrentVersion(t *testing.T) {
	got := Message("v1.13.0", []Entry{{Version: "v1.12.0", Notes: []string{"Twelve."}}})
	if !strings.Contains(got, "v1.13.0") || !strings.Contains(got, "\nv1.12.0\n") {
		t.Errorf("Message = %q, want the headline at v1.13.0 and the notes labelled v1.12.0", got)
	}
}

func TestMessageWithoutNotes(t *testing.T) {
	for _, entries := range [][]Entry{nil, {{Version: "v1.4.0"}}} {
		if got := Message("v1.4.0", entries); got != "🚀 Updated to v1.4.0" {
			t.Errorf("Message(%v) = %q, want the headline alone", entries, got)
		}
	}
}

func TestMessageAddsMissingVersionPrefix(t *testing.T) {
	if got := Message("1.4.0", nil); !strings.Contains(got, "v1.4.0") {
		t.Errorf("Message = %q, want a v-prefixed version", got)
	}
}

// The announcement is sent once, unchunked; an overlong changelog entry must
// be trimmed rather than rejected by Telegram.
func TestMessageTrimsOverlongNotes(t *testing.T) {
	notes := make([]string, 100)
	for i := range notes {
		notes[i] = strings.Repeat("x", 100)
	}

	got := Message("v1.4.0", []Entry{{Version: "v1.4.0", Notes: notes}})
	if n := len([]rune(got)); n > maxNotesRunes+100 {
		t.Errorf("message is %d runes, want it trimmed near %d", n, maxNotesRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("trimmed message = %q, want a trailing ellipsis", got[max(0, len(got)-40):])
	}
}

// A trim that swallows whole versions must say which ones, or the message
// silently claims to be the full story of the upgrade.
func TestMessageNamesTheVersionsItHadToDrop(t *testing.T) {
	long := make([]string, 100)
	for i := range long {
		long[i] = strings.Repeat("x", 100)
	}
	got := Message("v1.12.0", []Entry{
		{Version: "v1.12.0", Notes: long},
		{Version: "v1.11.0", Notes: []string{"Eleven."}},
		{Version: "v1.9.0", Notes: []string{"Nine."}},
	})

	if n := len([]rune(got)); n > maxNotesRunes+100 {
		t.Errorf("message is %d runes, want it trimmed near %d", n, maxNotesRunes)
	}
	if !strings.Contains(got, "v1.11.0") || !strings.Contains(got, "v1.9.0") {
		t.Errorf("trimmed message = %q, want the dropped versions named", got[max(0, len(got)-120):])
	}
}
