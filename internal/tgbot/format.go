package tgbot

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
)

// perPage is how many result buttons one keyboard page shows.
const perPage = 10

// maxButtonRunes caps a button label's length; Telegram clips longer labels.
const maxButtonRunes = 64

// Callback-data kinds (first segment of "<kind>:<searchID>:<n>").
const (
	cbDownload = "dl" // n = release index within the cached search
	cbPage     = "pg" // n = keyboard page to show
)

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

// buttonLabel renders one result button: "N. 45GB · 1200S · title…".
func buttonLabel(n int, r prowlarr.Release) string {
	label := fmt.Sprintf("%d. %s · %dS · %s", n, humanSize(r.Size), r.Seeders, r.Title)
	return truncate(label, maxButtonRunes)
}

// truncate limits s to max runes, replacing the tail with … when cut.
// Rune-based so mixed Cyrillic/Latin titles are never split mid-character.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
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

// resultsHeader is the message text shown above the result keyboard.
func resultsHeader(query string, total, page int) string {
	pages := numPages(total)
	if pages == 1 {
		return fmt.Sprintf("🔍 «%s»: %d result(s)", query, total)
	}
	return fmt.Sprintf("🔍 «%s»: %d result(s) — page %d/%d", query, total, page+1, pages)
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
