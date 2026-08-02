package mediainfo

import (
	"slices"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  Info
	}{
		{
			name:  "russian tracker style",
			title: "Космос / Space Show (2026) [BDRip 1080p, HEVC 10bit] MVO + AVO + Original Eng + Sub Rus, Eng",
			want: Info{
				Resolution:   "1080p",
				Source:       "BDRip",
				VideoCodec:   "HEVC",
				BitDepth:     10,
				Translations: []string{"MVO", "AVO", "Original"},
				AudioLangs:   []string{"Eng"},
				SubLangs:     []string{"Rus", "Eng"},
				HasSubs:      true,
			},
		},
		{
			name:  "scene style with glued channel layout",
			title: "Space.Show.2026.2160p.WEB-DL.DDP5.1.Atmos.HEVC-TRACKERA",
			want: Info{
				Resolution: "2160p",
				Source:     "WEB-DL",
				VideoCodec: "HEVC",
				Audio:      []string{"E-AC3", "Atmos"},
				Channels:   "5.1",
			},
		},
		{
			name:  "full remux with everything stated",
			title: "Космос / Space Show (2026) UHD BDRemux 2160p HDR10 Dolby Vision [Rus DTS-HD MA 7.1, Eng TrueHD Atmos] MKV ~85000 kbps",
			want: Info{
				Resolution: "2160p",
				Source:     "Remux",
				Container:  "MKV",
				HDR:        []string{"HDR10", "Dolby Vision"},
				Audio:      []string{"DTS-HD MA", "TrueHD", "Atmos"},
				Channels:   "7.1",
				AudioLangs: []string{"Rus", "Eng"},
				Bitrate:    "~85000 kbps",
			},
		},
		{
			name:  "old encode",
			title: "Space Show (2004) DVDRip [XviD, AC3] AVI Rus",
			want: Info{
				Source:     "DVDRip",
				VideoCodec: "XviD",
				Container:  "AVI",
				Audio:      []string{"AC3"},
				AudioLangs: []string{"Rus"},
			},
		},
		{
			name:  "cyrillic language abbreviations",
			title: "Космос: Полное собрание [2004, США, фэнтези, комедия, DVDRip] XviD, AC3, Рус, Англ",
			want: Info{
				Source:     "DVDRip",
				VideoCodec: "XviD",
				Audio:      []string{"AC3"},
				AudioLangs: []string{"Rus", "Eng"},
			},
		},
		{
			name:  "cyrillic bitrate unit and subtitle marker",
			title: "Космос [WEB-DL 1080p, x264] Дубляж, Русский + Субтитры Английский, 9500 Кбит/с",
			want: Info{
				Resolution:   "1080p",
				Source:       "WEB-DL",
				VideoCodec:   "H.264",
				Translations: []string{"Dub"},
				AudioLangs:   []string{"Rus"},
				SubLangs:     []string{"Eng"},
				HasSubs:      true,
				Bitrate:      "~9500 kbps",
			},
		},
		{
			name:  "megabit bitrate and bare subtitle marker",
			title: "Space Show 2026 1080i HDTV AVC AAC 2.0 + Sub, 10.5 Mbps",
			want: Info{
				Resolution: "1080i",
				Source:     "HDTV",
				VideoCodec: "H.264",
				Audio:      []string{"AAC"},
				Channels:   "2.0",
				HasSubs:    true,
				Bitrate:    "~10.5 Mbps",
			},
		},
		{
			name:  "3d release",
			title: "Space Show (2026) 3D BDRip 1080p Half-OU [AVC, DTS] Rus",
			want: Info{
				Resolution: "1080p",
				Source:     "BDRip",
				VideoCodec: "H.264",
				Audio:      []string{"DTS"},
				AudioLangs: []string{"Rus"},
				ThreeD:     true,
			},
		},
		{
			name:  "nothing recognisable",
			title: "Космос. Сезон 2026. Серия 5",
			want:  Info{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.title)
			if !equalInfo(got, tt.want) {
				t.Errorf("Parse(%q) =\n %+v\nwant\n %+v", tt.title, got, tt.want)
			}
		})
	}
}

func equalInfo(a, b Info) bool {
	return a.Resolution == b.Resolution &&
		a.Source == b.Source &&
		a.VideoCodec == b.VideoCodec &&
		a.BitDepth == b.BitDepth &&
		slices.Equal(a.HDR, b.HDR) &&
		a.Bitrate == b.Bitrate &&
		a.Container == b.Container &&
		slices.Equal(a.Audio, b.Audio) &&
		a.Channels == b.Channels &&
		slices.Equal(a.Translations, b.Translations) &&
		slices.Equal(a.AudioLangs, b.AudioLangs) &&
		slices.Equal(a.SubLangs, b.SubLangs) &&
		a.HasSubs == b.HasSubs &&
		a.ThreeD == b.ThreeD
}

func TestKnown(t *testing.T) {
	if Parse("Космос. Сезон 2026. Серия 5").Known() {
		t.Error("a title stating nothing should not be Known")
	}
	if !Parse("Космос [1080p]").Known() {
		t.Error("a title stating a resolution should be Known")
	}
}

func TestVideoAndAudioLines(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		wantVideo string
		wantAudio string
	}{
		{"codec with bit depth", "Космос [1080p, HEVC 10bit]", "HEVC 10bit", ""},
		{"codec without bit depth", "Космос [1080p, x264]", "H.264", ""},
		{"no codec", "Космос [1080p]", "", ""},
		{"channels ride the first codec", "Космос [DTS-HD MA 7.1, AC3]", "", "DTS-HD MA 7.1, AC3"},
		{"channels without a codec", "Космос [1080p] 5.1", "", "5.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := Parse(tt.title)
			if got := info.Video(); got != tt.wantVideo {
				t.Errorf("Video() = %q, want %q", got, tt.wantVideo)
			}
			if got := info.AudioLine(); got != tt.wantAudio {
				t.Errorf("AudioLine() = %q, want %q", got, tt.wantAudio)
			}
		})
	}
}

func TestAppleTV4K(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		wantLevel CompatLevel
		wantNote  bool
	}{
		{"hevc plays", "Космос [BDRip 1080p, HEVC] Rus", CompatOK, false},
		{"h264 plays", "Космос [BDRip 1080p, x264] Rus", CompatOK, false},
		{"xvid plays through infuse", "Космос [DVDRip, XviD] Rus", CompatOK, false},
		{"av1 1080p is a caution", "Космос [WEB-DL 1080p, AV1] Rus", CompatCaution, true},
		{"av1 4k is a caution", "Космос [WEB-DL 2160p, AV1] Rus", CompatCaution, true},
		{"3d is a caution", "Космос 3D [BDRip 1080p, AVC] Rus", CompatCaution, true},
		{"no codec is unknown", "Космос [BDRip 1080p] Rus", CompatUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.title).AppleTV4K()
			if got.Level != tt.wantLevel {
				t.Errorf("AppleTV4K().Level = %v, want %v", got.Level, tt.wantLevel)
			}
			if (got.Note != "") != tt.wantNote {
				t.Errorf("AppleTV4K().Note = %q, want note: %v", got.Note, tt.wantNote)
			}
		})
	}

	if a, b := Parse("Космос [2160p, AV1]").AppleTV4K(), Parse("Космос [1080p, AV1]").AppleTV4K(); a.Note == b.Note {
		t.Errorf("4K AV1 and 1080p AV1 should read differently, both say %q", a.Note)
	}
}
