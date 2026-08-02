package tgbot

import (
	"fmt"
	"strconv"
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
		{"no room at all", "hello", 0, ""},
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
	// A title stating its media: the label summarises the quality instead of
	// repeating a title the message text already shows in full.
	described := buttonLabel(3, prowlarr.Release{
		Title:    "Космос. Сезон 2026. Серия 5. Специальный выпуск. Финал [WEB-DL 1080p, x265, Rus] MKV",
		Size:     45 << 30, // 45 GiB
		Seeders:  1200,
		Leechers: 12,
	}, "")
	for _, want := range []string{"3. ", "45GB", "↑1200", "↓12", "1080p", "HEVC", "MKV"} {
		if !strings.Contains(described, want) {
			t.Errorf("label %q missing %q", described, want)
		}
	}

	// A title stating nothing: the label falls back to the title, which is
	// then all that distinguishes one result from another.
	bare := buttonLabel(1, prowlarr.Release{Title: "Short", Size: 700 << 20, Seeders: 5}, "")
	if bare != "1. 700MB · ↑5 · Short" {
		t.Errorf("bare label = %q", bare)
	}

	// Playback caution reaches the button too, not only the message text.
	caution := buttonLabel(2, prowlarr.Release{Title: "Space Show 2026 2160p WEB-DL AV1", Seeders: 3}, "")
	if !strings.Contains(caution, "⚠️") {
		t.Errorf("AV1 label %q should carry a caution mark", caution)
	}

	// Whatever the title, the label stays within Telegram's clipping length.
	long := buttonLabel(9, prowlarr.Release{
		Title:   strings.Repeat("Космос Специальный выпуск ", 20),
		Size:    1 << 30,
		Seeders: 7,
	}, "")
	if n := utf8.RuneCountInString(long); n > maxButtonRunes {
		t.Errorf("label has %d runes, want <= %d", n, maxButtonRunes)
	}
	if !strings.HasSuffix(long, "…") {
		t.Errorf("truncated label %q should end with ellipsis", long)
	}
}

func TestReleaseBlockShowsWhatTheTitleStates(t *testing.T) {
	block := releaseBlock(1, prowlarr.Release{
		Title:    "Космос / Space Show (2026) [UHD BDRemux 2160p, HEVC 10bit, HDR10] MKV, ~85000 kbps [Rus DTS-HD MA 7.1, Eng TrueHD Atmos] MVO + Original + Sub Rus, Eng",
		Size:     63 << 30,
		Seeders:  34,
		Leechers: 5,
		Indexer:  "TrackerA",
	}, "")

	for _, want := range []string{
		"1. Космос / Space Show (2026)", // full title, not clipped
		"63GB", "↑34", "↓5", "TrackerA",
		"2160p Remux", "HEVC 10bit", "HDR10", "MKV", "~85000 kbps",
		"Audio: DTS-HD MA 7.1, TrueHD, Atmos", "MVO, Original",
		"Subs: Rus, Eng",
		"Apple TV 4K: ✅",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}
}

func TestReleaseBlockOmitsUnknownFields(t *testing.T) {
	block := releaseBlock(2, prowlarr.Release{
		Title:   "Космос. Сезон 2026. Серия 5",
		Size:    523 << 20,
		Seeders: 28,
		Indexer: "TrackerB",
	}, "")

	want := "2. Космос. Сезон 2026. Серия 5\n" + blockIndent + "523MB · ↑28 · TrackerB"
	if block != want {
		t.Errorf("block =\n%s\nwant\n%s", block, want)
	}
	// No leecher count reported means no leecher count shown, not "↓0".
	if strings.Contains(block, "↓") {
		t.Errorf("block should not show a leecher count:\n%s", block)
	}
}

func TestReleaseBlockOmitsMissingSize(t *testing.T) {
	// Magnet-only results routinely arrive with no size. "0B" would read as
	// an empty torrent; zero seeders is real information and stays.
	block := releaseBlock(1, prowlarr.Release{Title: "Космос", Indexer: "TrackerB"}, "")
	if strings.Contains(block, "0B") {
		t.Errorf("block should omit an unreported size:\n%s", block)
	}
	if !strings.Contains(block, "↑0") || !strings.Contains(block, "TrackerB") {
		t.Errorf("block lost the swarm or indexer:\n%s", block)
	}
	if label := buttonLabel(1, prowlarr.Release{Title: "Космос"}, ""); strings.Contains(label, "0B") {
		t.Errorf("button label should omit an unreported size: %q", label)
	}
}

func TestReleaseBlockReportsPlaybackCaution(t *testing.T) {
	block := releaseBlock(1, prowlarr.Release{
		Title:   "Space Show 2026 2160p WEB-DL AV1 Opus",
		Seeders: 9,
	}, "")
	if !strings.Contains(block, "Apple TV 4K: ⚠️") || !strings.Contains(block, "AV1") {
		t.Errorf("block should flag AV1 playback:\n%s", block)
	}
}

func TestReleaseBlockReportsUnnamedSubtitles(t *testing.T) {
	block := releaseBlock(1, prowlarr.Release{Title: "Космос [BDRip 1080p, AVC] Rus + Sub", Seeders: 1}, "")
	if !strings.Contains(block, "Subs: yes") {
		t.Errorf("block should report subtitles of unstated language:\n%s", block)
	}
}

func TestResultsMessageFitsOneSend(t *testing.T) {
	// Worst case: a full page of pathologically long titles. The results
	// message carries the keyboard, so it cannot be chunked — it must fit.
	releases := make([]prowlarr.Release, perPage)
	for i := range releases {
		releases[i] = prowlarr.Release{
			Title:   strings.Repeat("Космос Специальный выпуск Финал ", 60),
			Size:    int64(i+1) << 30,
			Seeders: i,
			Indexer: "TrackerA",
		}
	}

	msg := resultsMessage(strings.Repeat("космос ", 200), releases, 0, nil)
	if n := utf16Len(msg); n > maxMessageUnits {
		t.Errorf("message has %d UTF-16 units, want <= %d", n, maxMessageUnits)
	}
	// Every result on the page keeps its number, so taps stay aligned with
	// the keyboard even when the text had to be cut.
	for i := 1; i <= perPage; i++ {
		if !strings.Contains(msg, fmt.Sprintf("%d. ", i)) {
			t.Errorf("message dropped result %d:\n%s", i, msg)
		}
	}

	// Astral characters cost two UTF-16 units each — the budget has to be
	// measured the way Telegram enforces it, not in runes.
	for i := range releases {
		releases[i].Title = strings.Repeat("🎬🚀", 900)
	}
	astral := resultsMessage("🎬", releases, 0, nil)
	if n := utf16Len(astral); n > maxMessageUnits {
		t.Errorf("astral message has %d UTF-16 units, want <= %d", n, maxMessageUnits)
	}
}

func TestUTF16Len(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"Космос", 6},
		{"…", 1},
		{"🎬", 2}, // one rune, two UTF-16 units
		{"a🎬b", 4},
	}
	for _, tt := range tests {
		if got := utf16Len(tt.in); got != tt.want {
			t.Errorf("utf16Len(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestTruncateUnits(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short unchanged", "hello", 10, "hello"},
		{"exact unchanged", "Космос", 6, "Космос"},
		{"cut with ellipsis", "hello world", 8, "hello w…"},
		{"no room at all", "hello", 0, ""},
		{"astral pair costs two", "🎬🎬🎬", 5, "🎬🎬…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateUnits(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("truncateUnits(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if n := utf16Len(got); n > tt.max {
				t.Errorf("result has %d units, want <= %d", n, tt.max)
			}
		})
	}
}

func TestResultsMessagePages(t *testing.T) {
	releases := make([]prowlarr.Release, perPage+2)
	for i := range releases {
		releases[i] = prowlarr.Release{Title: fmt.Sprintf("Space Show 2026 E%02d [1080p, HEVC]", i), Seeders: i}
	}

	first := resultsMessage("space show", releases, 0, nil)
	if !strings.Contains(first, "E00") || strings.Contains(first, fmt.Sprintf("E%02d", perPage)) {
		t.Errorf("page 1 shows the wrong slice:\n%s", first)
	}
	second := resultsMessage("space show", releases, 1, nil)
	if !strings.Contains(second, fmt.Sprintf("E%02d", perPage)) || strings.Contains(second, "E00") {
		t.Errorf("page 2 shows the wrong slice:\n%s", second)
	}
	// Numbering continues across pages so it matches the callback indexes.
	if !strings.Contains(second, fmt.Sprintf("%d. ", perPage+1)) {
		t.Errorf("page 2 should number results from %d:\n%s", perPage+1, second)
	}

	if got := resultsMessage("space show", nil, 0, nil); got != resultsHeader("space show", 0, 0) {
		t.Errorf("empty results message = %q, want just the header", got)
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
		{total: 1, page: 0, wantPages: 1, wantStart: 0, wantEnd: 1, wantClamped: 0},
		{total: perPage, page: 0, wantPages: 1, wantStart: 0, wantEnd: perPage, wantClamped: 0},
		{total: 2*perPage + 3, page: 0, wantPages: 3, wantStart: 0, wantEnd: perPage, wantClamped: 0},
		{total: 2*perPage + 3, page: 2, wantPages: 3, wantStart: 2 * perPage, wantEnd: 2*perPage + 3, wantClamped: 2},
		// Clamped down, then up.
		{total: 2*perPage + 3, page: 99, wantPages: 3, wantStart: 2 * perPage, wantEnd: 2*perPage + 3, wantClamped: 2},
		{total: 2*perPage + 3, page: -1, wantPages: 3, wantStart: 0, wantEnd: perPage, wantClamped: 0},
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
	single := resultsHeader("dune", perPage, 0)
	if !strings.Contains(single, "dune") || !strings.Contains(single, strconv.Itoa(perPage)) {
		t.Errorf("header %q missing query or count", single)
	}
	if strings.Contains(single, "page") {
		t.Errorf("single-page header %q should not mention pages", single)
	}

	multi := resultsHeader("dune", 2*perPage+5, 1)
	if !strings.Contains(multi, "2/3") {
		t.Errorf("multi-page header %q missing page 2/3", multi)
	}

	// A pasted wall of text must not crowd out the results below it.
	long := resultsHeader(strings.Repeat("космос ", 100), 3, 0)
	if n := utf8.RuneCountInString(long); n > maxHeaderQueryRunes+40 {
		t.Errorf("header for a long query has %d runes: %q", n, long)
	}
}
