package torrentmeta

import (
	"fmt"
	"strings"
	"testing"
)

// --- tiny bencode writers, so fixtures read as structure rather than as a
// hand-counted byte string (Cyrillic names make those counts error-prone). ---

func bstr(s string) string { return fmt.Sprintf("%d:%s", len(s), s) }

func bint(n int64) string { return fmt.Sprintf("i%de", n) }

func blist(items ...string) string { return "l" + strings.Join(items, "") + "e" }

// bdict writes key/value pairs in the order given; the decoder does not care
// about bencode's sorted-key rule, and neither do real-world torrents always.
func bdict(kv ...string) string { return "d" + strings.Join(kv, "") + "e" }

// singleFile is a one-file torrent, the shape most movie releases have.
func singleFile() string {
	return bdict(
		bstr("announce"), bstr("http://tracker-a.example.com/announce"),
		bstr("comment"), bstr("Космос, сезон 2026"),
		bstr("created by"), bstr("uTorrent/3.5.5"),
		bstr("info"), bdict(
			bstr("length"), bint(4831838208),
			bstr("name"), bstr("Космос.2026.1080p.mkv"),
			bstr("piece length"), bint(2097152),
			bstr("pieces"), bstr("\x00\x01\x02\x03"),
			bstr("private"), bint(1),
		),
	)
}

// multiFile is a season pack: a directory name plus per-file entries.
func multiFile() string {
	return bdict(
		bstr("info"), bdict(
			bstr("files"), blist(
				bdict(
					bstr("length"), bint(2147483648),
					bstr("path"), blist(bstr("Season 1"), bstr("E01.mkv")),
				),
				bdict(
					bstr("length"), bint(1073741824),
					bstr("path"), blist(bstr("Season 1"), bstr("E02.mkv")),
				),
				bdict(
					bstr("length"), bint(1048576),
					bstr("path"), blist(bstr("readme.txt")),
				),
			),
			bstr("name"), bstr("Space Show 2026"),
			bstr("pieces"), bstr("\x00\x01"),
		),
	)
}

func TestParseSingleFile(t *testing.T) {
	m, err := Parse([]byte(singleFile()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "Космос.2026.1080p.mkv" {
		t.Errorf("Name = %q", m.Name)
	}
	if len(m.Files) != 1 {
		t.Fatalf("Files = %v, want the single file itself", m.Files)
	}
	if m.Files[0].Path != m.Name || m.Files[0].Length != 4831838208 {
		t.Errorf("Files[0] = %+v", m.Files[0])
	}
	if m.TotalSize != 4831838208 {
		t.Errorf("TotalSize = %d", m.TotalSize)
	}
	if m.Comment != "Космос, сезон 2026" || m.CreatedBy != "uTorrent/3.5.5" {
		t.Errorf("Comment = %q, CreatedBy = %q", m.Comment, m.CreatedBy)
	}
	if !m.Private {
		t.Error("Private = false, want true")
	}
}

func TestParseMultiFile(t *testing.T) {
	m, err := Parse([]byte(multiFile()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "Space Show 2026" {
		t.Errorf("Name = %q", m.Name)
	}
	want := []File{
		{Path: "Season 1/E01.mkv", Length: 2147483648},
		{Path: "Season 1/E02.mkv", Length: 1073741824},
		{Path: "readme.txt", Length: 1048576},
	}
	if len(m.Files) != len(want) {
		t.Fatalf("Files = %+v, want %d entries", m.Files, len(want))
	}
	for i, f := range want {
		if m.Files[i] != f {
			t.Errorf("Files[%d] = %+v, want %+v", i, m.Files[i], f)
		}
	}
	if m.TotalSize != 2147483648+1073741824+1048576 {
		t.Errorf("TotalSize = %d", m.TotalSize)
	}
	if m.Private {
		t.Error("Private = true, want false when the key is absent")
	}
}

func TestParsePrefersUTF8Variants(t *testing.T) {
	// Legacy clients wrote a mojibake name/path in the plain key and the real
	// one in the ".utf-8" twin; the Cyrillic releases this bot searches for are
	// exactly where that shows up.
	data := bdict(
		bstr("info"), bdict(
			bstr("files"), blist(
				bdict(
					bstr("length"), bint(10),
					bstr("path"), blist(bstr("Ceзoн"), bstr("legacy.mkv")),
					bstr("path.utf-8"), blist(bstr("Сезон 1"), bstr("Этап 14.mkv")),
				),
			),
			bstr("name"), bstr("Kocmoc"),
			bstr("name.utf-8"), bstr("Космос"),
		),
	)

	m, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "Космос" {
		t.Errorf("Name = %q, want the utf-8 variant", m.Name)
	}
	if got := m.Files[0].Path; got != "Сезон 1/Этап 14.mkv" {
		t.Errorf("Files[0].Path = %q, want the utf-8 variant", got)
	}
}

func TestParseKeepsPiecesOutOfTheWay(t *testing.T) {
	// The pieces string is by far the largest field in a real .torrent and the
	// one field nothing here needs; parsing must not choke on its raw bytes.
	pieces := strings.Repeat("\x00\xff\xfe", 20000)
	data := bdict(
		bstr("info"), bdict(
			bstr("length"), bint(1),
			bstr("name"), bstr("x.mkv"),
			bstr("pieces"), bstr(pieces),
		),
	)

	m, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "x.mkv" {
		t.Errorf("Name = %q", m.Name)
	}
}

func TestParseErrors(t *testing.T) {
	deepList := strings.Repeat("l", 200) + strings.Repeat("e", 200)

	tests := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"not a dict", blist(bstr("a"))},
		{"truncated dict", "d4:info"},
		{"truncated string", "d4:info5:ab"},
		{"unterminated integer", bdict(bstr("info"), "i123")},
		{"bad integer", bdict(bstr("info"), "iabce")},
		{"trailing garbage", singleFile() + "junk"},
		{"non-string key", "di1e4:spame"},
		{"missing info", bdict(bstr("comment"), bstr("hi"))},
		{"info not a dict", bdict(bstr("info"), bstr("hi"))},
		{"missing name", bdict(bstr("info"), bdict(bstr("length"), bint(1)))},
		{"empty name", bdict(bstr("info"), bdict(bstr("name"), bstr(""), bstr("length"), bint(1)))},
		{"no length and no files", bdict(bstr("info"), bdict(bstr("name"), bstr("x")))},
		{"negative length", bdict(bstr("info"), bdict(bstr("name"), bstr("x"), bstr("length"), bint(-5)))},
		{"file entry not a dict", bdict(bstr("info"), bdict(
			bstr("name"), bstr("x"),
			bstr("files"), blist(bstr("nope")),
		))},
		{"file entry without length", bdict(bstr("info"), bdict(
			bstr("name"), bstr("x"),
			bstr("files"), blist(bdict(bstr("path"), blist(bstr("a.mkv")))),
		))},
		{"file entry with empty path", bdict(bstr("info"), bdict(
			bstr("name"), bstr("x"),
			bstr("files"), blist(bdict(bstr("length"), bint(1), bstr("path"), blist())),
		))},
		{"file entry with non-string path part", bdict(bstr("info"), bdict(
			bstr("name"), bstr("x"),
			bstr("files"), blist(bdict(bstr("length"), bint(1), bstr("path"), blist(bint(3)))),
		))},
		{"empty files list", bdict(bstr("info"), bdict(
			bstr("name"), bstr("x"),
			bstr("files"), blist(),
		))},
		{"nesting bomb", bdict(bstr("info"), deepList)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m, err := Parse([]byte(tt.data)); err == nil {
				t.Fatalf("Parse: want error, got %+v", m)
			}
		})
	}
}
