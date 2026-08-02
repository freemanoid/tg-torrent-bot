// Package release announces an update in the chat. On the Umbrel app store an
// update lands as a silent container restart, so the user has no way of
// telling that a new version arrived or what it brought. Once per version this
// package posts "updated to X" plus that version's CHANGELOG.md entry.
package release

import (
	"context"
	"fmt"
	"log/slog"
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

	if err := a.notifier.Notify(ctx, Message(a.version, Notes(a.changelog, a.version))); err != nil {
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
// changelog entry as a bullet list when there is one. A version with no
// changelog entry still gets the headline — that the update landed is the
// larger half of the news.
func Message(version string, notes []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚀 Updated to %s", displayVersion(version))
	if len(notes) == 0 {
		return b.String()
	}

	b.WriteString("\n\nWhat's new:")
	budget := maxNotesRunes
	for _, note := range notes {
		budget -= utf8.RuneCountInString(note) + len("\n• ")
		if budget < 0 {
			b.WriteString("\n…")
			break
		}
		b.WriteString("\n• " + note)
	}
	return b.String()
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

// Notes extracts the list items under the `## <version>` heading of a
// changelog. Unknown versions yield no notes rather than an error: shipping a
// tag without a changelog entry is sloppy, not broken. Wrapped continuation
// lines are folded back into their item, so the changelog can stay readable at
// 80 columns.
func Notes(changelog, version string) []string {
	want := normalizeVersion(version)
	if want == "" {
		return nil
	}

	var notes []string
	inEntry := false
	for _, line := range strings.Split(changelog, "\n") {
		trimmed := strings.TrimSpace(line)

		if heading, ok := entryHeading(trimmed); ok {
			if inEntry {
				break // the next version's entry starts here
			}
			inEntry = normalizeVersion(heading) == want
			continue
		}
		if !inEntry {
			continue
		}
		if item, ok := listItem(trimmed); ok {
			notes = append(notes, item)
			continue
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && len(notes) > 0 {
			notes[len(notes)-1] += " " + trimmed
		}
	}
	return notes
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
