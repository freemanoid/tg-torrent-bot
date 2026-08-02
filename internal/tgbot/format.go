package tgbot

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/freemanoid/tg-torrent-bot/internal/mediainfo"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
)

// perPage is how many results one page shows. Each one now carries its full
// title and several lines of media detail, so five is what stays readable on a
// phone — and what safely fits maxMessageLen (see resultsMessage).
const perPage = 5

// maxButtonRunes caps a button label's length; Telegram clips longer labels.
const maxButtonRunes = 64

// maxMessageUnits is the room resultsMessage may use, in the UTF-16 code units
// Telegram actually counts. It sits just below the 4096 limit: the results
// message carries the inline keyboard and so cannot be split, and overflowing
// it would lose a search that may have taken minutes to run.
const maxMessageUnits = 4000

// blockIndent aligns a result's detail lines under its numbered title.
const blockIndent = "   "

// Callback-data kinds (first segment of "<kind>:<searchID>:<n>").
const (
	cbDownload = "dl" // n = release index within the cached search
	cbPage     = "pg" // n = keyboard page to show
)

// Markers stating what the bot already did with a release. They reuse the
// glyphs the add confirmation and /status already use, so download state reads
// the same everywhere in the chat.
const (
	markActive = "⬇️" // handed to Transmission, still downloading
	markDone   = "✅"  // finished
)

// statusMark renders a stored download status as its marker, and nothing for a
// status the store never writes.
func statusMark(status string) string {
	switch status {
	case store.StatusActive:
		return markActive
	case store.StatusDone:
		return markDone
	default:
		return ""
	}
}

// humanSize renders a byte count compactly: "45GB", "1.4GB", "700MB", "500KB".
func humanSize(bytes int64) string {
	const (
		kb = int64(1) << 10
		mb = kb << 10
		gb = mb << 10
	)
	v := float64(bytes)
	switch {
	case bytes >= 10*gb:
		return fmt.Sprintf("%.0fGB", v/float64(gb))
	case bytes >= gb:
		return fmt.Sprintf("%.1fGB", v/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.0fMB", v/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.0fKB", v/float64(kb))
	default:
		return strconv.FormatInt(bytes, 10) + "B"
	}
}

// sizeLabel renders the release size, or nothing when the indexer omitted it —
// "0B" would read as an empty torrent rather than as the missing field it is.
// Zero *seeders*, by contrast, is a fact worth showing, so swarm keeps those.
func sizeLabel(r prowlarr.Release) string {
	if r.Size <= 0 {
		return ""
	}
	return humanSize(r.Size)
}

// swarm renders the swarm health: "↑1200 ↓12", dropping the leecher count
// when the indexer did not report one.
func swarm(r prowlarr.Release) string {
	s := "↑" + strconv.Itoa(r.Seeders)
	if r.Leechers > 0 {
		s += " ↓" + strconv.Itoa(r.Leechers)
	}
	return s
}

// buttonLabel renders one result button. A button label is one clipped line,
// so it carries only what makes a tap unambiguous — the quality summary from
// the block above it, or the title when the release said nothing about its
// media. Everything else lives in the message text. mark, when set, leads the
// label: truncation eats the tail, so a prefix is the one position that always
// survives.
func buttonLabel(n int, r prowlarr.Release, mark string) string {
	info := mediainfo.Parse(r.Title)
	tail := r.Title
	if q := joinNonEmpty(" ", info.Resolution, info.VideoCodec, info.Container, cautionMark(info)); q != "" {
		tail = q
	}
	body := joinNonEmpty(" · ", sizeLabel(r), swarm(r), tail)
	label := fmt.Sprintf("%d. %s", n, joinNonEmpty(" ", mark, body))
	return truncate(label, maxButtonRunes)
}

// releaseBlock renders one numbered result as it appears in the message text:
// the full title, then only those detail lines the title actually stated.
// Nothing is invented — a release that names no codec simply shows no codec
// line, which is more useful than a row of "unknown" placeholders. mark, when
// set, leads the title line so it survives the block's rune budget.
func releaseBlock(n int, r prowlarr.Release, mark string) string {
	info := mediainfo.Parse(r.Title)
	lines := []string{fmt.Sprintf("%d. %s", n, joinNonEmpty(" ", mark, r.Title))}
	add := func(s string) {
		if s != "" {
			lines = append(lines, blockIndent+s)
		}
	}

	add(joinNonEmpty(" · ", sizeLabel(r), swarm(r), r.Indexer))
	add(joinNonEmpty(" · ",
		joinNonEmpty(" ", info.Resolution, info.Source),
		info.Video(),
		strings.Join(info.HDR, " "),
		info.Container,
		info.Bitrate))
	add(audioLine(info))
	add(subsLine(info))
	add(compatLine(info))

	return strings.Join(lines, "\n")
}

// audioLine reports the two axes a tracker title states separately: which
// codecs are in the file, and which translations and languages they carry.
func audioLine(info mediainfo.Info) string {
	body := joinNonEmpty(" · ",
		info.AudioLine(),
		strings.Join(info.Translations, ", "),
		strings.Join(info.AudioLangs, ", "))
	if body == "" {
		return ""
	}
	return "Audio: " + body
}

// subsLine reports subtitles, distinguishing named languages from a bare
// "+ Sub" that promises subtitles without saying which.
func subsLine(info mediainfo.Info) string {
	switch {
	case len(info.SubLangs) > 0:
		return "Subs: " + strings.Join(info.SubLangs, ", ")
	case info.HasSubs:
		return "Subs: yes (language not stated)"
	default:
		return ""
	}
}

// compatLine states the Apple TV 4K verdict, and says nothing at all when the
// title named no video codec rather than guessing one.
func compatLine(info mediainfo.Info) string {
	switch c := info.AppleTV4K(); c.Level {
	case mediainfo.CompatOK:
		return "Apple TV 4K: ✅ plays (Infuse/Plex)"
	case mediainfo.CompatCaution:
		return "Apple TV 4K: ⚠️ " + c.Note
	default:
		return ""
	}
}

// cautionMark is the warning sign a button carries when playback needs a
// second look, and empty otherwise.
func cautionMark(info mediainfo.Info) string {
	if info.AppleTV4K().Level == mediainfo.CompatCaution {
		return "⚠️"
	}
	return ""
}

// joinNonEmpty joins the parts that are not empty, so an unknown field leaves
// no dangling separator behind.
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// truncate limits s to max runes, replacing the tail with … when cut.
// Rune-based so mixed Cyrillic/Latin titles are never split mid-character.
func truncate(s string, max int) string {
	if max < 1 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

// utf16Len measures s the way Telegram enforces its message limit: in UTF-16
// code units, where anything outside the Basic Multilingual Plane — an emoji
// in a release title, say — counts as two rather than one.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// truncateUnits is truncate measured in UTF-16 code units, for the one place
// where the difference decides whether a message sends at all.
func truncateUnits(s string, max int) string {
	if max < 1 {
		return ""
	}
	if utf16Len(s) <= max {
		return s
	}
	used := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if used+w > max-1 { // one unit held back for the ellipsis
			return s[:i] + "…"
		}
		used += w
	}
	return s
}

// numPages is how many keyboard pages total results occupy (at least 1).
func numPages(total int) int {
	if total <= 0 {
		return 1
	}
	return (total + perPage - 1) / perPage
}

// clampPage keeps page within [0, numPages(total)-1].
func clampPage(total, page int) int {
	if last := numPages(total) - 1; page > last {
		page = last
	}
	if page < 0 {
		page = 0
	}
	return page
}

// pageBounds returns the half-open result range [start, end) shown on page.
func pageBounds(total, page int) (start, end int) {
	start = clampPage(total, page) * perPage
	return start, min(start+perPage, total)
}

// maxHeaderQueryRunes caps how much of the query the header echoes, so a
// pasted wall of text cannot eat the message budget the results need.
const maxHeaderQueryRunes = 80

// resultsHeader is the first line of the results message.
func resultsHeader(query string, total, page int) string {
	query = truncate(query, maxHeaderQueryRunes)
	pages := numPages(total)
	if pages == 1 {
		return fmt.Sprintf("🔍 «%s»: %d result(s)", query, total)
	}
	return fmt.Sprintf("🔍 «%s»: %d result(s) — page %d/%d", query, total, page+1, pages)
}

// resultsMessage renders a whole results page: the header plus one detail
// block per result on it.
//
// The inline keyboard rides on this one message, so unlike h.reply's output it
// cannot be split across several — it has to fit in a single send or the send
// fails and a search that may have taken minutes is lost. Blocks therefore
// share a rune budget rather than being dropped: keeping every block keeps the
// numbering aligned with the buttons even when a title is pathologically long.
//
// marks carries the download-state marker per release index, and may be nil —
// a lookup that failed simply renders the results the way it always did.
func resultsMessage(query string, releases []prowlarr.Release, page int, marks map[int]string) string {
	page = clampPage(len(releases), page)
	start, end := pageBounds(len(releases), page)
	header := resultsHeader(query, len(releases), page)
	if start >= end {
		return header
	}

	const sepUnits = 2 // the blank line between blocks
	shown := end - start
	budget := (maxMessageUnits - utf16Len(header) - sepUnits*shown) / shown

	parts := make([]string, 0, shown+1)
	parts = append(parts, header)
	for i := start; i < end; i++ {
		parts = append(parts, truncateUnits(releaseBlock(i+1, releases[i], marks[i]), budget))
	}
	return strings.Join(parts, "\n\n")
}

// encodeCallback packs a kind, search ID, and index into callback data; with
// 8-character IDs the result stays far below Telegram's 64-byte limit.
func encodeCallback(kind, searchID string, n int) string {
	return kind + ":" + searchID + ":" + strconv.Itoa(n)
}

// decodeCallback splits callback data produced by encodeCallback.
func decodeCallback(data string) (kind, searchID string, n int, err error) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return "", "", 0, fmt.Errorf("bad callback data %q", data)
	}
	n, err = strconv.Atoi(parts[2])
	if err != nil {
		return "", "", 0, fmt.Errorf("bad callback data %q", data)
	}
	return parts[0], parts[1], n, nil
}
