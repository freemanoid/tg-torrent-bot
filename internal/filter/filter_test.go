package filter

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Filter
	}{
		{
			name: "full example from plan",
			in:   "rus, 1080p, x265, -720p, -/cam|ts/, >1gb, <30gb",
			want: Filter{
				Include:   []string{"rus", "1080p", "x265"},
				Exclude:   []string{"720p", "/cam|ts/"},
				MinSizeMB: 1024,
				MaxSizeMB: 30720,
			},
		},
		{
			name: "empty string",
			in:   "",
			want: Filter{},
		},
		{
			name: "whitespace only",
			in:   "   \t ",
			want: Filter{},
		},
		{
			name: "empty tokens between commas skipped",
			in:   "rus,,1080p, ,x265",
			want: Filter{Include: []string{"rus", "1080p", "x265"}},
		},
		{
			name: "include regex kept in filter syntax",
			in:   "/космос/",
			want: Filter{Include: []string{"/космос/"}},
		},
		{
			name: "exclude regex stored without leading dash",
			in:   "-/x26[45]/",
			want: Filter{Exclude: []string{"/x26[45]/"}},
		},
		{
			name: "size bounds in mb",
			in:   ">700mb, <1400mb",
			want: Filter{MinSizeMB: 700, MaxSizeMB: 1400},
		},
		{
			name: "size units are case-insensitive",
			in:   ">1GB, <500Mb",
			want: Filter{MinSizeMB: 1024, MaxSizeMB: 500},
		},
		{
			name: "decimal gigabytes",
			in:   ">1.5gb",
			want: Filter{MinSizeMB: 1536},
		},
		{
			name: "later size bound wins",
			in:   ">1gb, >2gb",
			want: Filter{MinSizeMB: 2048},
		},
		{
			name: "cyrillic substring token",
			in:   "космос, -тизер",
			want: Filter{Include: []string{"космос"}, Exclude: []string{"тизер"}},
		},
		{
			name: "extra whitespace trimmed",
			in:   "  rus ,  -720p  ,  >1gb ",
			want: Filter{Include: []string{"rus"}, Exclude: []string{"720p"}, MinSizeMB: 1024},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tt.in, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string // substring the error message must contain
	}{
		{name: "bad include regex", in: "/[/", wantErr: "regex"},
		{name: "bad exclude regex", in: "-/(/", wantErr: "regex"},
		{name: "size without number", in: ">gb", wantErr: "size"},
		{name: "size with bad number", in: ">abcgb", wantErr: "size"},
		{name: "size with unknown unit", in: ">10tb", wantErr: "size"},
		{name: "negative size", in: ">-1gb", wantErr: "size"},
		{name: "zero size", in: "<0mb", wantErr: "size"},
		{name: "lone dash", in: "-", wantErr: "empty exclude"},
		{name: "bad token among good ones", in: "rus, /[/, 1080p", wantErr: "regex"},
		{name: "comma inside include regex", in: "/x26[4,5]/", wantErr: "unterminated regex"},
		{name: "comma inside exclude regex", in: "-/cam,ts/", wantErr: "unterminated regex"},
		{name: "comma inside regex repetition", in: "/x26{4,5}/", wantErr: "unterminated regex"},
		{name: "lone slash", in: "/", wantErr: "unterminated regex"},
		{name: "size overflowing int64 mebibytes", in: ">9999999999999gb", wantErr: "too large"},
		{name: "nan size", in: ">nangb", wantErr: "positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.in)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error containing %q", tt.in, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Parse(%q) error = %q, want it to contain %q", tt.in, err, tt.wantErr)
			}
		})
	}
}

const gib = int64(1) << 30

func TestMatch(t *testing.T) {
	const (
		ssRus1080 = "Космос. Сезон 2026. Серия 1 / Space Show [2026, WEB-DL 1080p, x265, Rus + Eng]"
		ssEng720  = "Space.Show.2026.Episode01.720p.WEB-DL.H264"
		movieCam  = "Дом Дракона / House of the Dragon (2026) [CAMRip 1080p, Rus]"
	)
	tests := []struct {
		name      string
		filter    string
		title     string
		sizeBytes int64
		want      bool
	}{
		{
			name:      "empty filter matches everything",
			filter:    "",
			title:     ssEng720,
			sizeBytes: 0,
			want:      true,
		},
		{
			name:      "all includes present case-insensitively",
			filter:    "rus, 1080p, x265",
			title:     ssRus1080, // title has "Rus", filter has "rus"
			sizeBytes: 4 * gib,
			want:      true,
		},
		{
			name:      "missing include fails",
			filter:    "rus, 1080p, x265",
			title:     ssEng720,
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "cyrillic include matches case-insensitively",
			filter:    "космос", // title has "Космос"
			title:     ssRus1080,
			sizeBytes: 4 * gib,
			want:      true,
		},
		{
			name:      "exclude substring rejects",
			filter:    "-720p",
			title:     ssEng720,
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "exclude substring case-insensitive",
			filter:    "-camrip", // title has "CAMRip"
			title:     movieCam,
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "regex include matches",
			filter:    "/x26[45]/",
			title:     ssRus1080,
			sizeBytes: 4 * gib,
			want:      true,
		},
		{
			name:      "regex include no match",
			filter:    "/x26[45]/",
			title:     ssEng720, // H264, not x264/x265
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "regex exclude rejects",
			filter:    "-/camrip|telesync/",
			title:     movieCam,
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "regex is case-insensitive",
			filter:    "/CAMRIP/",
			title:     movieCam,
			sizeBytes: 4 * gib,
			want:      true,
		},
		{
			name:      "size within bounds",
			filter:    ">1gb, <30gb",
			title:     ssRus1080,
			sizeBytes: 4 * gib,
			want:      true,
		},
		{
			name:      "size below lower bound",
			filter:    ">10gb",
			title:     ssRus1080,
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "size above upper bound",
			filter:    "<2gb",
			title:     ssRus1080,
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "size exactly at strict lower bound fails",
			filter:    ">1gb",
			title:     ssRus1080,
			sizeBytes: 1 * gib,
			want:      false,
		},
		{
			name:      "unknown size fails lower bound",
			filter:    ">1gb",
			title:     ssRus1080,
			sizeBytes: 0,
			want:      false,
		},
		{
			name:      "unknown size passes upper-bound-only filter",
			filter:    "<30gb",
			title:     ssRus1080,
			sizeBytes: 0,
			want:      true,
		},
		{
			name:      "combined: includes pass but exclude rejects",
			filter:    "rus, 1080p, -camrip",
			title:     movieCam,
			sizeBytes: 4 * gib,
			want:      false,
		},
		{
			name:      "combined: everything passes",
			filter:    "космос, 1080p, x265, -720p, -/cam|telesync/, >1gb, <30gb",
			title:     ssRus1080,
			sizeBytes: 4 * gib,
			want:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse(tt.filter)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tt.filter, err)
			}
			if got := f.Match(tt.title, tt.sizeBytes); got != tt.want {
				t.Errorf("Parse(%q).Match(%q, %d) = %v, want %v",
					tt.filter, tt.title, tt.sizeBytes, got, tt.want)
			}
		})
	}
}

// TestMatchReconstructedFromStore simulates rebuilding a Filter from persisted
// subscription fields (raw token slices + size bounds) rather than Parse.
func TestMatchReconstructedFromStore(t *testing.T) {
	f := Filter{
		Include:   []string{"rus", "/x26[45]/"},
		Exclude:   []string{"720p"},
		MinSizeMB: 1024,
		MaxSizeMB: 30720,
	}
	title := "Космос [2026, WEB-DL 1080p, x265, Rus]"
	if !f.Match(title, 4*gib) {
		t.Errorf("Match(%q) = false, want true", title)
	}
	if f.Match("Space Show [2026, 720p, x265, Rus]", 4*gib) {
		t.Error("Match with excluded 720p = true, want false")
	}
}

// TestMatchInvalidRegexNeverMatches: a hand-built Filter with a broken regex
// must fail closed (include never satisfied) instead of panicking.
func TestMatchInvalidRegexNeverMatches(t *testing.T) {
	f := Filter{Include: []string{"/[/"}}
	if f.Match("anything", 0) {
		t.Error("Match with invalid include regex = true, want false")
	}
	f = Filter{Exclude: []string{"/[/"}}
	if !f.Match("anything", 0) {
		t.Error("Match with invalid exclude regex = false, want true (exclude ignored)")
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		name string
		f    Filter
		want string
	}{
		{
			name: "empty",
			f:    Filter{},
			want: "",
		},
		{
			name: "full",
			f: Filter{
				Include:   []string{"rus", "1080p", "x265"},
				Exclude:   []string{"720p", "/cam|ts/"},
				MinSizeMB: 1024,
				MaxSizeMB: 30720,
			},
			want: "rus, 1080p, x265, -720p, -/cam|ts/, >1gb, <30gb",
		},
		{
			name: "non-gigabyte sizes rendered in mb",
			f:    Filter{MinSizeMB: 700, MaxSizeMB: 1536},
			want: ">700mb, <1536mb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseStringRoundTrip: String output must parse back to an equal Filter.
func TestParseStringRoundTrip(t *testing.T) {
	in := "rus, 1080p, x265, -720p, -/cam|ts/, >1gb, <30gb"
	f1, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", in, err)
	}
	f2, err := Parse(f1.String())
	if err != nil {
		t.Fatalf("Parse(String()) error = %v", err)
	}
	if !reflect.DeepEqual(f1, f2) {
		t.Errorf("round trip mismatch: %+v != %+v", f1, f2)
	}
}
