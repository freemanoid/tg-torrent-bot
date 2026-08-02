package tgbot

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
)

func TestHumanSize(t *testing.T) {
	const (
		kb = int64(1) << 10
		mb = kb << 10
		gb = mb << 10
	)
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0B"},
		{999, "999B"},
		{500 * kb, "500KB"},
		{700 * mb, "700MB"},
		{gb + gb/2, "1.5GB"},
		{45 * gb, "45GB"},
		{1503238554, "1.4GB"}, // 1.4 GiB
	}
	for _, tt := range tests {
		if got := humanSize(tt.bytes); got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"exact unchanged", "hello", 5, "hello"},
		{"cut with ellipsis", "hello world", 8, "hello w…"},
		{"cyrillic rune-safe", "Космос Серия 5", 9, "Космос С…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if n := utf8.RuneCountInString(got); n > tt.max {
				t.Errorf("result has %d runes, want <= %d", n, tt.max)
			}
		})
	}
}

func TestButtonLabel(t *testing.T) {
	r := prowlarr.Release{
		Title:   "Космос. Сезон 2026. Серия 5. Специальный выпуск. Финал [WEB-DL 1080p, x265, Rus] — очень длинное название релиза",
		Size:    45 << 30, // 45 GiB
		Seeders: 1200,
	}
	label := buttonLabel(3, r)

	for _, want := range []string{"3. ", "45GB", "1200S", "Космос"} {
		if !strings.Contains(label, want) {
			t.Errorf("label %q missing %q", label, want)
		}
	}
	if n := utf8.RuneCountInString(label); n > maxButtonRunes {
		t.Errorf("label has %d runes, want <= %d", n, maxButtonRunes)
	}
	if !strings.HasSuffix(label, "…") {
		t.Errorf("truncated label %q should end with ellipsis", label)
	}

	short := buttonLabel(1, prowlarr.Release{Title: "Short", Size: 700 << 20, Seeders: 5})
	if short != "1. 700MB · 5S · Short" {
		t.Errorf("short label = %q", short)
	}
}

func TestPaginationMath(t *testing.T) {
	tests := []struct {
		total, page        int
		wantPages          int
		wantStart, wantEnd int
		wantClamped        int
	}{
		{total: 0, page: 0, wantPages: 1, wantStart: 0, wantEnd: 0, wantClamped: 0},
		{total: 5, page: 0, wantPages: 1, wantStart: 0, wantEnd: 5, wantClamped: 0},
		{total: 10, page: 0, wantPages: 1, wantStart: 0, wantEnd: 10, wantClamped: 0},
		{total: 25, page: 0, wantPages: 3, wantStart: 0, wantEnd: 10, wantClamped: 0},
		{total: 25, page: 2, wantPages: 3, wantStart: 20, wantEnd: 25, wantClamped: 2},
		{total: 25, page: 99, wantPages: 3, wantStart: 20, wantEnd: 25, wantClamped: 2}, // clamped down
		{total: 25, page: -1, wantPages: 3, wantStart: 0, wantEnd: 10, wantClamped: 0},  // clamped up
	}
	for _, tt := range tests {
		if got := numPages(tt.total); got != tt.wantPages {
			t.Errorf("numPages(%d) = %d, want %d", tt.total, got, tt.wantPages)
		}
		if got := clampPage(tt.total, tt.page); got != tt.wantClamped {
			t.Errorf("clampPage(%d, %d) = %d, want %d", tt.total, tt.page, got, tt.wantClamped)
		}
		start, end := pageBounds(tt.total, tt.page)
		if start != tt.wantStart || end != tt.wantEnd {
			t.Errorf("pageBounds(%d, %d) = %d..%d, want %d..%d",
				tt.total, tt.page, start, end, tt.wantStart, tt.wantEnd)
		}
	}
}

func TestCallbackRoundTrip(t *testing.T) {
	tests := []struct {
		kind string
		id   string
		n    int
	}{
		{cbDownload, "a1b2c3d4", 0},
		{cbDownload, "ffffffff", 49},
		{cbPage, "00000000", 99},
	}
	for _, tt := range tests {
		data := encodeCallback(tt.kind, tt.id, tt.n)
		if len(data) > 64 {
			t.Errorf("encodeCallback(%q, %q, %d) = %d bytes, exceeds Telegram's 64-byte limit",
				tt.kind, tt.id, tt.n, len(data))
		}
		kind, id, n, err := decodeCallback(data)
		if err != nil {
			t.Fatalf("decodeCallback(%q): %v", data, err)
		}
		if kind != tt.kind || id != tt.id || n != tt.n {
			t.Errorf("round trip %q -> (%q, %q, %d), want (%q, %q, %d)",
				data, kind, id, n, tt.kind, tt.id, tt.n)
		}
	}
}

func TestDecodeCallbackErrors(t *testing.T) {
	for _, data := range []string{"", "dl", "dl:abc", "dl:abc:x", "dl:abc:1:2"} {
		if _, _, _, err := decodeCallback(data); err == nil {
			t.Errorf("decodeCallback(%q) succeeded, want error", data)
		}
	}
}

func TestResultsHeader(t *testing.T) {
	single := resultsHeader("dune", 7, 0)
	if !strings.Contains(single, "dune") || !strings.Contains(single, "7") {
		t.Errorf("header %q missing query or count", single)
	}
	if strings.Contains(single, "page") {
		t.Errorf("single-page header %q should not mention pages", single)
	}

	multi := resultsHeader("dune", 25, 1)
	if !strings.Contains(multi, "2/3") {
		t.Errorf("multi-page header %q missing page 2/3", multi)
	}
}
