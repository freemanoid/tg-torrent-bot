// Package release announces an update in the chat. On the Umbrel app store an
// update lands as a silent container restart, so the user has no way of
// telling that a new version arrived or what it brought. Once per version this
// package posts "updated to X" plus that version's CHANGELOG.md entry.
package release

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	tgtorrentbot "github.com/freemanoid/tg-torrent-bot"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
)

// DevVersion is what Version reports when no version was injected at build
// time — a local `go build`/`go run`. Such a build never announces anything:
// it has no release to talk about, and a developer restarting the binary all
// day should not spam the chat.
const DevVersion = "dev"

// Version is the release this binary was built from ("v1.4.0"). The Docker
// build injects the git tag with
//
//	-ldflags "-X github.com/freemanoid/tg-torrent-bot/internal/release.Version=$VERSION"
//
// It is deliberately the tag rather than the newest CHANGELOG.md heading:
// the tag is what actually shipped, and a docs-only edit to the changelog must
// not make the binary claim to be a version that was never released.
var Version = DevVersion

// announcedKey is the meta key holding the last version announced to the user.
// Storing it in the database (rather than, say, a file) is what makes the
// announcement survive restarts and fire exactly once per update.
const announcedKey = "announced_version"

// maxNotesRunes caps the changelog part of the message. Telegram rejects
// anything over 4096 characters, and this message is sent once at startup with
// no chunking, so a long entry is trimmed rather than lost.
const maxNotesRunes = 2000

// Store is the persistence surface the announcer uses; *store.Store implements
// it, tests fake it.
type Store interface {
	Meta(ctx context.Context, key string) (string, error)
	SetMeta(ctx context.Context, key, value string) error
	FreshDatabase() bool
}

var _ Store = (*store.Store)(nil)

// Notifier delivers a message to the user (in production: a Telegram message
// to the allowed chat).
type Notifier interface {
	Notify(ctx context.Context, text string) error
}

// Announcer posts the one-off update message.
type Announcer struct {
	store     Store
	notifier  Notifier
	version   string
	changelog string
	log       *slog.Logger
}

// NewAnnouncer wires an announcer for the version this binary was built from
// and the changelog embedded alongside it. A nil log falls back to
// slog.Default().
func NewAnnouncer(st Store, n Notifier, log *slog.Logger) *Announcer {
	if log == nil {
		log = slog.Default()
	}
	return &Announcer{
		store:     st,
		notifier:  n,
		version:   Version,
		changelog: tgtorrentbot.Changelog,
		log:       log,
	}
}

// Announce sends the update message unless it has already been sent for this
// version, and records the version only once Telegram accepted it — a failed
// send is retried on the next start rather than swallowed. A brand-new install
// records the version silently: there was no update to report.
func (a *Announcer) Announce(ctx context.Context) error {
	if !released(a.version) {
		a.log.Debug("no release announcement: this binary has no version", "version", a.version)
		return nil
	}

	last, err := a.store.Meta(ctx, announcedKey)
	if err != nil {
		return fmt.Errorf("read announced version: %w", err)
	}
	if last == a.version {
		return nil
	}
	if last == "" && a.store.FreshDatabase() {
		a.log.Info("first run, recording version without announcing", "version", a.version)
		return a.store.SetMeta(ctx, announcedKey, a.version)
	}

	if err := a.notifier.Notify(ctx, Message(a.version, NotesSince(a.changelog, last, a.version))); err != nil {
		return fmt.Errorf("send release announcement: %w", err)
	}
	// The message is out, so the version must be recorded even if the process
	// is already shutting down — otherwise a cancel landing in this window
	// would repeat the announcement on the next start.
	if err := a.store.SetMeta(context.WithoutCancel(ctx), announcedKey, a.version); err != nil {
		// The message is already out; failing to record it only means it is
		// sent once more after the next restart.
		return fmt.Errorf("record announced version: %w", err)
	}
	a.log.Info("announced release", "version", a.version, "previous", last)
	return nil
}

// released reports whether version names an actual release rather than an
// uninjected local build.
func released(version string) bool {
	v := strings.TrimSpace(version)
	return v != "" && v != DevVersion
}

// Message renders the announcement: a headline naming the version, and the
// changelog entries as bullet lists when there are any. A version with no
// changelog entry still gets the headline — that the update landed is the
// larger half of the news.
//
// Several entries are each headed by their own version, because an upgrade
// that crossed more than one release is otherwise indistinguishable from a
// single one with a lot to say. A single entry for the version being announced
// needs no such heading: the headline already named it.
func Message(version string, entries []Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚀 Updated to %s", displayVersion(version))

	entries = withNotes(entries)
	if len(entries) == 0 {
		return b.String()
	}
	label := len(entries) > 1 || normalizeVersion(entries[0].Version) != normalizeVersion(version)

	b.WriteString("\n\nWhat's new:")
	budget := maxNotesRunes
	// dropped names the entries the budget could not fit at all; a bare "…"
	// would not say that a whole version went missing.
	var dropped []string
	for i, entry := range entries {
		if label {
			head := "\n\n" + displayVersion(entry.Version)
			if budget-utf8.RuneCountInString(head) < 0 {
				dropped = versionsOf(entries[i:])
				break
			}
			budget -= utf8.RuneCountInString(head)
			b.WriteString(head)
		}
		truncated := false
		for _, note := range entry.Notes {
			budget -= utf8.RuneCountInString(note) + len("\n• ")
			if budget < 0 {
				truncated = true
				break
			}
			b.WriteString("\n• " + note)
		}
		if truncated {
			dropped = versionsOf(entries[i+1:])
			break
		}
	}
	if budget < 0 || len(dropped) > 0 {
		b.WriteString("\n…")
		if len(dropped) > 0 {
			b.WriteString(" and the notes for " + strings.Join(dropped, ", "))
		}
	}
	return b.String()
}

// withNotes drops entries that would render as an empty section.
func withNotes(entries []Entry) []Entry {
	kept := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if len(e.Notes) > 0 {
			kept = append(kept, e)
		}
	}
	return kept
}

// versionsOf lists the entries' versions as they are displayed.
func versionsOf(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, displayVersion(e.Version))
	}
	return out
}

// displayVersion prefixes a bare "1.4.0" with "v" so the message reads the
// same whether the tag carried the prefix or not.
func displayVersion(version string) string {
	v := strings.TrimSpace(version)
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}
	return v
}

// Entry is one changelog entry: the version its heading names and the list
// items written under it.
type Entry struct {
	Version string
	Notes   []string
}

// Entries parses the whole changelog in one pass, in the order it is written
// (newest first, by convention). It is the only reader of the markdown —
// Notes and NotesSince are lookups over its result, so the two cannot drift.
// Wrapped continuation lines are folded back into their item, so the changelog
// can stay readable at 80 columns.
func Entries(changelog string) []Entry {
	var entries []Entry
	for _, line := range strings.Split(changelog, "\n") {
		trimmed := strings.TrimSpace(line)

		if heading, ok := entryHeading(trimmed); ok {
			// A headingless `##` gets an entry too: it names no version, so
			// nothing matches it, but it still ends the previous entry.
			entries = append(entries, Entry{Version: heading})
			continue
		}
		if len(entries) == 0 {
			continue // preamble, before any entry
		}
		current := &entries[len(entries)-1]
		if item, ok := listItem(trimmed); ok {
			current.Notes = append(current.Notes, item)
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && len(current.Notes) > 0 {
			current.Notes[len(current.Notes)-1] += " " + trimmed
		}
	}
	return entries
}

// Notes extracts the list items under the `## <version>` heading of a
// changelog. Unknown versions yield no notes rather than an error: shipping a
// tag without a changelog entry is sloppy, not broken.
func Notes(changelog, version string) []string {
	want := normalizeVersion(version)
	if want == "" {
		return nil
	}
	for _, entry := range Entries(changelog) {
		if normalizeVersion(entry.Version) == want {
			return entry.Notes
		}
	}
	return nil
}

// NotesSince returns every changelog entry released after `from` and no later
// than `to`, newest first. Umbrel applies updates one container restart at a
// time, but a user who let a few releases pile up crosses all of them at once
// — announcing only `to` would silently swallow the rest.
//
// A range that says nothing useful — no version recorded yet, a rollback, a
// version this changelog has never heard of — falls back to `to`'s own entry,
// which is exactly what was sent before ranges existed. In particular an empty
// `from` must not dump the entire changelog on a database that predates the
// announcement feature.
func NotesSince(changelog, from, to string) []Entry {
	all := Entries(changelog)
	current := func() []Entry {
		for _, entry := range all {
			if normalizeVersion(entry.Version) == normalizeVersion(to) {
				return []Entry{entry}
			}
		}
		return nil
	}

	if cmp, ok := compareVersions(from, to); !ok || cmp >= 0 {
		return current()
	}

	var span []Entry
	for _, entry := range all {
		afterFrom, ok := compareVersions(entry.Version, from)
		if !ok {
			continue
		}
		upToTo, ok := compareVersions(entry.Version, to)
		if !ok {
			continue
		}
		if afterFrom > 0 && upToTo <= 0 {
			span = append(span, entry)
		}
	}
	if len(span) == 0 {
		return current()
	}
	// The file is written newest first, but the message must not depend on
	// whoever last edited it having kept that order.
	sort.SliceStable(span, func(i, j int) bool {
		cmp, _ := compareVersions(span[i].Version, span[j].Version)
		return cmp > 0
	})
	return span
}

// compareVersions orders two dotted-numeric versions, reporting ok=false for
// anything it cannot read that way. A string compare would put "1.9.0" above
// "1.11.0" and hide a release; an unreadable version is better treated as
// unknown than as smaller.
func compareVersions(a, b string) (int, bool) {
	left, ok := versionParts(a)
	if !ok {
		return 0, false
	}
	right, ok := versionParts(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < len(left) || i < len(right); i++ {
		var l, r int
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if l != r {
			if l < r {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// versionParts splits "v1.12.0" into its numeric segments.
func versionParts(v string) ([]int, bool) {
	v = normalizeVersion(v)
	if v == "" {
		return nil, false
	}
	fields := strings.Split(v, ".")
	parts := make([]int, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}

// entryHeading returns the version named by a `## …` heading.
func entryHeading(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "##")
	if !ok || strings.HasPrefix(rest, "#") {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", true // a headingless `##` still ends the previous entry
	}
	return fields[0], true
}

// listItem returns the text of a markdown list item.
func listItem(line string) (string, bool) {
	for _, marker := range []string{"- ", "* ", "• "} {
		if rest, ok := strings.CutPrefix(line, marker); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// normalizeVersion makes heading and tag spellings comparable: "## [v1.4.0]"
// and "1.4.0" both name the same release.
func normalizeVersion(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.Trim(v, "[]")
	return strings.TrimPrefix(v, "v")
}
