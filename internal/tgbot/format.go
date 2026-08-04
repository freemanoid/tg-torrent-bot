package tgbot

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/freemanoid/tg-torrent-bot/internal/mediainfo"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
	"github.com/freemanoid/tg-torrent-bot/internal/store"
	"github.com/freemanoid/tg-torrent-bot/internal/torrentmeta"
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

// Callback-data kinds (first segment of "<kind>:<ref>:<n>"). For most kinds
// ref is a cached search's id; the reject kinds carry an info hash instead,
// because the notification they sit on outlives any search — see
// HandleCallback. A 40-character hash still leaves the whole payload well
// inside Telegram's 64-byte cap on callback data.
const (
	cbDownload = "dl" // ref = search, n = release index within it
	cbPage     = "pg" // ref = search, n = keyboard page to show
	cbInfo     = "if" // ref = search, n = release index to describe in full
	cbSub      = "sb" // ref = search, n unused: subscribe to that search's query
	cbReject   = "rj" // ref = info hash, n unused: offer to undo a grab
	cbRejectOK = "ro" // ref = info hash, n unused: undo confirmed
	cbRejectNo = "rn" // ref = info hash, n unused: undo abandoned
	// The /status buttons need their own kinds rather than reusing the reject
	// ones: those swap the keyboard on the message they sit on, and a /status
	// message carries one button per download, so an unlabelled yes/no in their
	// place would not say which entry it meant.
	cbStatusDel   = "sd" // ref = info hash, n unused: offer to delete a listed download
	cbStatusDelOK = "so" // ref = info hash, n unused: deletion confirmed
	cbStatusDelNo = "sn" // ref = info hash, n unused: deletion abandoned
)

// Markers stating what the bot already did with a release. They reuse the
// glyphs the add confirmation and /status already use, so download state reads
// the same everywhere in the chat.
const (
	markActive = "⬇️" // handed to Transmission, still downloading
	markDone   = "✅"  // finished
)

// statusMark renders a stored download status as its marker, and nothing for a
// status the store never writes. A cancelled download is deliberately
// unmarked: the user rejected it and its data is gone, so showing it as
// downloading or downloaded in a later search would be a lie.
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
	lines := []string{fmt.Sprintf("%d. %s", n, joinNonEmpty(" ", mark, r.Title))}
	for _, l := range releaseLines(r) {
		lines = append(lines, blockIndent+l)
	}
	return strings.Join(lines, "\n")
}

// releaseLines are the detail lines both views share: what the release weighs
// and where it came from, then what its title says about the media. Empty
// lines are dropped, so an unstated field leaves no blank row behind.
func releaseLines(r prowlarr.Release) []string {
	info := mediainfo.Parse(r.Title)
	candidates := []string{
		joinNonEmpty(" · ", sizeLabel(r), swarm(r), r.Indexer),
		joinNonEmpty(" · ",
			joinNonEmpty(" ", info.Resolution, info.Source),
			info.Video(),
			strings.Join(info.HDR, " "),
			info.Container,
			info.Bitrate),
		audioLine(info),
		subsLine(info),
		compatLine(info),
	}
	lines := make([]string, 0, len(candidates))
	for _, l := range candidates {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
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

// maxLinkURLBytes caps a link button's URL. Telegram states no limit, but a
// URL it refuses fails the whole send — and the send is a search that may have
// taken minutes. An indexer publishing kilobytes of "URL" gets no button.
const maxLinkURLBytes = 2048

// linkURL is the release's page on the tracker, when it can be published as a
// link button, and empty otherwise — hence the "when possible" in what the
// keyboard offers.
//
// The value is indexer-supplied and reaches Telegram unescaped, so it is
// checked rather than trusted: Telegram answers BUTTON_URL_INVALID for
// anything that is not an absolute http(s) URL and rejects the entire message,
// which would cost the search that produced it. A relative link or a bare
// hostname — both of which trackers do publish — is therefore no link here.
func linkURL(r prowlarr.Release) string {
	raw := strings.TrimSpace(r.InfoURL)
	if raw == "" || len(raw) > maxLinkURLBytes {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return raw
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

// Details view budgets, all in the UTF-16 code units Telegram counts. Like the
// results message, the details message carries an inline keyboard and so
// cannot be split across sends — it has to fit in one message or it is lost.
const (
	maxFileLines        = 30   // file rows shown before the "and N more" tail
	maxFilePathUnits    = 110  // one file path, so a deep tree cannot eat the page
	maxDescriptionUnits = 700  // the indexer's free text
	maxDetailTitleUnits = 400  // the release title, shown in full unless absurd
	filesReserveUnits   = 1600 // room held back for the file list before the head is cut
)

// publishFormat renders a release's publish date; the time of day says nothing
// useful about a tracker upload.
const publishFormat = "2006-01-02"

// detailsHeader is the first line of the details message.
func detailsHeader(n int, r prowlarr.Release, mark string) string {
	title := truncateUnits(r.Title, maxDetailTitleUnits)
	return fmt.Sprintf("ℹ️ %d. %s", n, joinNonEmpty(" ", mark, title))
}

// provenanceLines are the facts the indexer states about the release rather
// than the media: when it was published, how often it was taken, and where its
// page is. Prowlarr fills these in per indexer, so most are often absent.
func provenanceLines(r prowlarr.Release) []string {
	var lines []string
	published := ""
	if !r.PublishDate.IsZero() {
		published = "Published " + r.PublishDate.UTC().Format(publishFormat)
	}
	grabs := ""
	if r.Grabs > 0 {
		grabs = fmt.Sprintf("%d grab(s)", r.Grabs)
	}
	if l := joinNonEmpty(" · ", published, grabs); l != "" {
		lines = append(lines, l)
	}
	if r.InfoURL != "" {
		lines = append(lines, r.InfoURL)
	}
	return lines
}

// detailsMessage renders everything known about one release before it is
// downloaded: the untruncated title, every detail line, what the indexer says
// about it, and the file list read out of the .torrent.
//
// meta is nil when the metainfo could not be read — the common case for a
// magnet-only release, whose Prowlarr downloadUrl redirects to a magnet the
// HTTP fetch cannot follow. unavailable then explains why, and the view still
// shows everything else: the download itself will work through the magnet.
func detailsMessage(n int, r prowlarr.Release, mark string, meta *torrentmeta.Meta, unavailable string) string {
	lines := []string{detailsHeader(n, r, mark)}
	for _, l := range releaseLines(r) {
		lines = append(lines, blockIndent+l)
	}
	for _, l := range provenanceLines(r) {
		lines = append(lines, blockIndent+l)
	}
	head := strings.Join(lines, "\n")
	if d := strings.TrimSpace(r.Description); d != "" {
		head += "\n\n" + truncateUnits(d, maxDescriptionUnits)
	}

	reserve := filesReserveUnits
	if meta == nil {
		reserve = 200 // just the one "unavailable" line
	}
	head = truncateUnits(head, maxMessageUnits-reserve)

	files := filesSection(r, meta, unavailable, maxMessageUnits-utf16Len(head)-2)
	if files == "" {
		return head
	}
	return head + "\n\n" + files
}

// filesSection renders the file list within budget, always leaving room for
// the line that says how many files were left out — dropping that line is how
// a truncated list turns into a lie about what the torrent contains.
func filesSection(r prowlarr.Release, meta *torrentmeta.Meta, unavailable string, budget int) string {
	if meta == nil {
		return truncateUnits(joinNonEmpty(" ", "📁", indexerFileCount(r), unavailable), budget)
	}

	header := fmt.Sprintf("📁 %d file(s) · %s", len(meta.Files), humanSize(meta.TotalSize))
	used := utf16Len(header)
	shown := make([]string, 0, min(len(meta.Files), maxFileLines))
	for i, f := range meta.Files {
		if i == maxFileLines {
			break
		}
		line := blockIndent + truncateUnits(f.Path, maxFilePathUnits) + " — " + humanSize(f.Length)
		// Room for this line plus the tail it would leave behind; the tail can
		// only get shorter as fewer files remain, so this never under-reserves.
		tail := moreFilesLine(len(meta.Files) - i - 1)
		if used+1+utf16Len(line)+1+utf16Len(tail) > budget {
			break
		}
		used += 1 + utf16Len(line)
		shown = append(shown, line)
	}

	out := header
	if len(shown) > 0 {
		out += "\n" + strings.Join(shown, "\n")
	}
	if tail := moreFilesLine(len(meta.Files) - len(shown)); tail != "" {
		out += "\n" + tail
	}
	return out
}

// indexerFileCount reports the file count Prowlarr carries, for the releases
// whose metainfo could not be read at all.
func indexerFileCount(r prowlarr.Release) string {
	if r.FileCount <= 0 {
		return ""
	}
	return fmt.Sprintf("%d file(s) —", r.FileCount)
}

// moreFilesLine states how many files the list left out, and nothing when it
// left out none.
func moreFilesLine(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%s… and %d more file(s)", blockIndent, n)
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
