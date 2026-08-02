// Package mediainfo extracts what a torrent release title says about the media
// inside it: resolution, source, video and audio codecs, translations,
// subtitles, container and bitrate.
//
// None of this is structured data. Prowlarr returns a title and a size; every
// tracker encodes the rest in the title itself, in one of two conventions —
// scene style ("Space.Show.2026.2160p.WEB-DL.HEVC.DDP5.1-GROUP") and the much
// richer Russian tracker style ("Space Show (2026) [BDRip 1080p, HEVC] MVO +
// AVO + Sub Rus, Eng"). Parsing is therefore best-effort by design: every
// field is optional, and a title that says nothing yields a zero Info rather
// than a guess. Callers render only the fields that came back non-empty.
package mediainfo

import (
	"regexp"
	"strconv"
	"strings"
)

// Info is everything a release title revealed. Every field is optional; use
// Known to tell "nothing recognised" apart from "recognised as empty".
type Info struct {
	Resolution   string   // "2160p", "1080p", "720p"
	Source       string   // "Remux", "BDRip", "WEB-DL", "HDTV", "DVDRip"…
	VideoCodec   string   // "HEVC", "H.264", "AV1", "XviD"…
	BitDepth     int      // 8, 10, 12; 0 when unstated
	HDR          []string // "HDR10", "Dolby Vision", "HLG"
	Bitrate      string   // as written, normalised: "~9500 kbps"
	Container    string   // "MKV", "MP4", "AVI", "ISO", "BDMV"…
	Audio        []string // codecs: "DTS-HD MA", "AC3", "Atmos"…
	Channels     string   // richest layout seen: "5.1", "7.1"
	Translations []string // "Dub", "MVO", "DVO", "AVO", "VO", "Original"
	AudioLangs   []string // "Rus", "Eng"… languages outside a subtitle run
	SubLangs     []string // languages listed after a "Sub" marker
	HasSubs      bool     // a subtitle marker was present at all
	ThreeD       bool
}

// Known reports whether the title yielded anything worth showing.
func (i Info) Known() bool {
	return i.Resolution != "" || i.Source != "" || i.VideoCodec != "" ||
		i.Container != "" || i.Bitrate != "" || i.BitDepth != 0 ||
		len(i.HDR) > 0 || len(i.Audio) > 0 || i.Channels != "" ||
		len(i.Translations) > 0 || len(i.AudioLangs) > 0 ||
		i.HasSubs || i.ThreeD
}

// Video renders the video codec with its bit depth: "HEVC 10bit".
func (i Info) Video() string {
	switch {
	case i.VideoCodec == "":
		return ""
	case i.BitDepth == 0:
		return i.VideoCodec
	default:
		return i.VideoCodec + " " + strconv.Itoa(i.BitDepth) + "bit"
	}
}

// AudioLine renders codecs and channel layout together: "DTS-HD MA 5.1, AC3".
// The layout is attached to the first (richest) codec, which is how tracker
// titles write it and how a reader expects to see it.
func (i Info) AudioLine() string {
	if len(i.Audio) == 0 {
		if i.Channels == "" {
			return ""
		}
		return i.Channels
	}
	out := append([]string(nil), i.Audio...)
	if i.Channels != "" {
		out[0] += " " + i.Channels
	}
	return strings.Join(out, ", ")
}

// CompatLevel grades how a release is expected to play back.
type CompatLevel int

const (
	CompatUnknown CompatLevel = iota // the title never named a video codec
	CompatOK                         // plays as-is
	CompatCaution                    // plays, but with a caveat worth reading
)

// Compat is a playback verdict plus the reason behind it.
type Compat struct {
	Level CompatLevel
	Note  string // short reason; empty when Level is CompatOK
}

// AppleTV4K reports how the release is expected to play on an Apple TV 4K
// through a third-party player (Infuse, Plex, VLC) rather than the built-in
// TV app. That profile is the one worth reporting: the built-in player rejects
// MKV and DTS outright, which would mark nearly every tracker release
// unplayable and tell the reader nothing. Under Infuse the container and audio
// codec stop mattering and only the video codec can bite — AV1, which the
// Apple TV 4K has no hardware decoder for, and 3D, which nothing on tvOS
// presents properly.
func (i Info) AppleTV4K() Compat {
	if i.ThreeD {
		return Compat{CompatCaution, "3D — no tvOS support"}
	}
	switch i.VideoCodec {
	case "":
		return Compat{Level: CompatUnknown}
	case "AV1":
		if i.Resolution == "2160p" {
			return Compat{CompatCaution, "AV1 4K — software decode, expect stutter"}
		}
		return Compat{CompatCaution, "AV1 — software decode"}
	default:
		return Compat{Level: CompatOK}
	}
}

// field names the Info member a recognised phrase feeds.
type field int

const (
	fResolution field = iota
	fSource
	fVideo
	fContainer
	fAudio
	fHDR
	fTranslation
	fLang
	fBits
	f3D
)

type phrase struct {
	f field
	v string
}

// phrases maps a lowercase, space-joined token run to the fact it states.
// Longer runs are matched first, so "dts hd ma" wins over "dts".
var phrases = map[string]phrase{
	// Resolution.
	"2160p": {fResolution, "2160p"}, "4k": {fResolution, "2160p"},
	"uhd": {fResolution, "2160p"}, "3840x2160": {fResolution, "2160p"},
	"1080p": {fResolution, "1080p"}, "1920x1080": {fResolution, "1080p"},
	"1080i": {fResolution, "1080i"},
	"720p":  {fResolution, "720p"}, "1280x720": {fResolution, "720p"},
	"576p": {fResolution, "576p"}, "480p": {fResolution, "480p"},

	// Source.
	"bdremux": {fSource, "Remux"}, "bd remux": {fSource, "Remux"},
	"remux": {fSource, "Remux"},
	"bdrip": {fSource, "BDRip"}, "bd rip": {fSource, "BDRip"}, "brrip": {fSource, "BDRip"},
	"bluray": {fSource, "Blu-ray"}, "blu ray": {fSource, "Blu-ray"},
	"web dl": {fSource, "WEB-DL"}, "webdl": {fSource, "WEB-DL"},
	"webrip": {fSource, "WEBRip"}, "web rip": {fSource, "WEBRip"},
	"hdtvrip": {fSource, "HDTVRip"}, "hdtv": {fSource, "HDTV"},
	"hdrip": {fSource, "HDRip"}, "tvrip": {fSource, "TVRip"},
	"dvdrip": {fSource, "DVDRip"}, "dvd5": {fSource, "DVD5"}, "dvd9": {fSource, "DVD9"},
	"dvdscr": {fSource, "DVDScr"}, "satrip": {fSource, "SATRip"},
	"camrip": {fSource, "CamRip"}, "telesync": {fSource, "TeleSync"},

	// Video codec.
	"hevc": {fVideo, "HEVC"}, "x265": {fVideo, "HEVC"},
	"h.265": {fVideo, "HEVC"}, "h265": {fVideo, "HEVC"}, "h 265": {fVideo, "HEVC"},
	"avc": {fVideo, "H.264"}, "x264": {fVideo, "H.264"},
	"h.264": {fVideo, "H.264"}, "h264": {fVideo, "H.264"}, "h 264": {fVideo, "H.264"},
	"av1": {fVideo, "AV1"}, "vp9": {fVideo, "VP9"},
	"xvid": {fVideo, "XviD"}, "divx": {fVideo, "DivX"},
	"vc 1": {fVideo, "VC-1"}, "vc1": {fVideo, "VC-1"},
	"mpeg 2": {fVideo, "MPEG-2"}, "mpeg2": {fVideo, "MPEG-2"},

	// Container.
	"mkv": {fContainer, "MKV"}, "matroska": {fContainer, "MKV"},
	"mp4": {fContainer, "MP4"}, "m4v": {fContainer, "MP4"},
	"avi": {fContainer, "AVI"}, "mov": {fContainer, "MOV"},
	"iso": {fContainer, "ISO"}, "bdmv": {fContainer, "BDMV"},
	"m2ts": {fContainer, "M2TS"}, "vob": {fContainer, "VOB"},
	"webm": {fContainer, "WebM"},

	// Audio codec.
	"dts hd ma": {fAudio, "DTS-HD MA"}, "dts hd": {fAudio, "DTS-HD"},
	"dts x": {fAudio, "DTS:X"}, "dts": {fAudio, "DTS"},
	"truehd": {fAudio, "TrueHD"}, "true hd": {fAudio, "TrueHD"},
	"atmos": {fAudio, "Atmos"}, "dolby atmos": {fAudio, "Atmos"},
	"eac3": {fAudio, "E-AC3"}, "e ac3": {fAudio, "E-AC3"}, "ddp": {fAudio, "E-AC3"},
	"ac3": {fAudio, "AC3"}, "dolby digital": {fAudio, "AC3"}, "dd": {fAudio, "AC3"},
	"aac": {fAudio, "AAC"}, "flac": {fAudio, "FLAC"}, "mp3": {fAudio, "MP3"},
	"opus": {fAudio, "Opus"}, "pcm": {fAudio, "PCM"}, "lpcm": {fAudio, "PCM"},

	// Dynamic range.
	"hdr10": {fHDR, "HDR10"}, "hdr": {fHDR, "HDR"}, "hlg": {fHDR, "HLG"},
	"dolby vision": {fHDR, "Dolby Vision"}, "dovi": {fHDR, "Dolby Vision"},

	// Translation kind — the axis Russian trackers list beside the audio codec.
	"dub": {fTranslation, "Dub"}, "dubbed": {fTranslation, "Dub"},
	"дубляж": {fTranslation, "Dub"}, "дублированный": {fTranslation, "Dub"},
	"mvo": {fTranslation, "MVO"}, "многоголосый": {fTranslation, "MVO"},
	"многоголосный": {fTranslation, "MVO"},
	"dvo":           {fTranslation, "DVO"}, "двухголосый": {fTranslation, "DVO"},
	"двухголосный": {fTranslation, "DVO"},
	"avo":          {fTranslation, "AVO"}, "авторский": {fTranslation, "AVO"},
	"vo": {fTranslation, "VO"}, "одноголосый": {fTranslation, "VO"},
	"одноголосный": {fTranslation, "VO"}, "закадровый": {fTranslation, "VO"},
	"original": {fTranslation, "Original"}, "оригинал": {fTranslation, "Original"},

	// Languages. Only unambiguous three-letter-and-longer forms: two-letter
	// codes ("it", "de", "es") collide with ordinary words in release titles.
	"rus": {fLang, "Rus"}, "russian": {fLang, "Rus"},
	"рус": {fLang, "Rus"}, "русский": {fLang, "Rus"},
	"eng": {fLang, "Eng"}, "english": {fLang, "Eng"},
	"англ": {fLang, "Eng"}, "английский": {fLang, "Eng"},
	"ukr": {fLang, "Ukr"}, "ukrainian": {fLang, "Ukr"},
	"укр": {fLang, "Ukr"}, "украинский": {fLang, "Ukr"},
	"jpn": {fLang, "Jpn"}, "jap": {fLang, "Jpn"}, "japanese": {fLang, "Jpn"},
	"япон": {fLang, "Jpn"}, "японский": {fLang, "Jpn"},
	"fra": {fLang, "Fra"}, "fre": {fLang, "Fra"}, "french": {fLang, "Fra"},
	"фран": {fLang, "Fra"}, "французский": {fLang, "Fra"},
	"ger": {fLang, "Ger"}, "deu": {fLang, "Ger"}, "german": {fLang, "Ger"},
	"нем": {fLang, "Ger"}, "немецкий": {fLang, "Ger"},
	"ita": {fLang, "Ita"}, "italian": {fLang, "Ita"},
	"итал": {fLang, "Ita"}, "итальянский": {fLang, "Ita"},
	"spa": {fLang, "Spa"}, "esp": {fLang, "Spa"}, "spanish": {fLang, "Spa"},
	"исп": {fLang, "Spa"}, "испанский": {fLang, "Spa"},
	"chi": {fLang, "Chi"}, "chn": {fLang, "Chi"}, "chinese": {fLang, "Chi"},
	"кит": {fLang, "Chi"}, "китайский": {fLang, "Chi"},
	"kor": {fLang, "Kor"}, "korean": {fLang, "Kor"},
	"кор": {fLang, "Kor"}, "корейский": {fLang, "Kor"},
	"pol": {fLang, "Pol"}, "polish": {fLang, "Pol"},
	"поль": {fLang, "Pol"}, "польский": {fLang, "Pol"},
	"tur": {fLang, "Tur"}, "turkish": {fLang, "Tur"},
	"тур": {fLang, "Tur"}, "турецкий": {fLang, "Tur"},

	// Misc.
	"10bit": {fBits, "10"}, "10 bit": {fBits, "10"},
	"8bit": {fBits, "8"}, "8 bit": {fBits, "8"},
	"12bit": {fBits, "12"}, "12 bit": {fBits, "12"},
	"3d": {f3D, ""},
}

// maxPhraseTokens is the longest token run in phrases ("dts hd ma").
const maxPhraseTokens = 3

// subMarkers introduce a run of subtitle languages.
var subMarkers = map[string]bool{
	"sub": true, "subs": true, "subtitles": true,
	"суб": true, "субтитры": true, "сабы": true,
}

var (
	// tokenRE splits a normalised title into words. dotHolder is part of a
	// word so the two dots worth keeping survive it.
	tokenRE = regexp.MustCompile(`[\p{L}\p{N}\x{0000}]+`)

	// A dot is a separator in scene naming ("Space.Show.2026.1080p") but part
	// of the word in a channel layout ("DDP5.1") and an H.26x codec name.
	// These two patterns stand the keepers aside as dotHolder before the rest
	// are flattened to spaces.
	protectChannels = regexp.MustCompile(`(^|[^0-9])([2-8])\.([01])([^0-9]|$)`)
	protectHCodec   = regexp.MustCompile(`(^|[^0-9\p{L}])(h)\.(26[45])`)

	// glued splits a codec written flush against its channel layout,
	// "DD5.1" and "DTS5.1", into the two facts it states.
	glued = regexp.MustCompile(`^(\p{L}+\d?)([2-8]\.[01])$`)

	// channelRE recognises a standalone channel layout token.
	channelRE = regexp.MustCompile(`^([2-8])\.([01])$`)

	// bitrateRE finds a stated bitrate anywhere in the raw title.
	bitrateRE = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*(kbps|kbit/s|kb/s|кбит/с|mbps|mbit/s|mb/s|мбит/с)`)
)

// dotHolder stands in for a dot that belongs to the word rather than between
// words. It is a control character, so no real title can contain it.
const dotHolder = "\x00"

// tokenize lowercases a title and splits it into comparable tokens.
func tokenize(title string) []string {
	s := strings.ToLower(title)
	s = strings.ReplaceAll(s, dotHolder, " ")
	s = protectChannels.ReplaceAllString(s, "${1}${2}"+dotHolder+"${3}${4}")
	s = protectHCodec.ReplaceAllString(s, "${1}${2}"+dotHolder+"${3}")

	raw := tokenRE.FindAllString(s, -1)
	toks := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.ReplaceAll(t, dotHolder, ".")
		if m := glued.FindStringSubmatch(t); m != nil {
			toks = append(toks, m[1], m[2])
			continue
		}
		toks = append(toks, t)
	}
	return toks
}

// Parse reads a release title. An unrecognised title yields a zero Info.
func Parse(title string) Info {
	toks := tokenize(title)
	var info Info

	// Subtitles first. A language run following a "Sub" marker describes the
	// subtitles, not the audio — "MVO, AVO, Sub Rus, Eng" states both at once
	// and only the marker's position tells them apart. Claimed indices are
	// then skipped by the audio-language pass below.
	claimed := make([]bool, len(toks))
	for n, t := range toks {
		if !subMarkers[t] {
			continue
		}
		info.HasSubs = true
		claimed[n] = true
		for m := n + 1; m < len(toks); m++ {
			p, ok := phrases[toks[m]]
			if !ok || p.f != fLang {
				break
			}
			info.SubLangs = appendUnique(info.SubLangs, p.v)
			claimed[m] = true
		}
	}

	for n := 0; n < len(toks); n++ {
		if claimed[n] {
			continue
		}
		if channelRE.MatchString(toks[n]) {
			info.Channels = richerChannels(info.Channels, toks[n])
			continue
		}
		p, width, ok := lookup(toks, n)
		if !ok {
			continue
		}
		apply(&info, p)
		n += width - 1
	}

	info.Bitrate = parseBitrate(title)
	return info
}

// lookup finds the longest phrase starting at toks[n], returning it and how
// many tokens it spans.
func lookup(toks []string, n int) (phrase, int, bool) {
	for w := min(maxPhraseTokens, len(toks)-n); w > 0; w-- {
		if p, ok := phrases[strings.Join(toks[n:n+w], " ")]; ok {
			return p, w, true
		}
	}
	return phrase{}, 0, false
}

// apply records one recognised phrase. Single-valued fields keep the first
// match, which in release titles is the more specific one.
func apply(info *Info, p phrase) {
	switch p.f {
	case fResolution:
		setFirst(&info.Resolution, p.v)
	case fSource:
		setFirst(&info.Source, p.v)
	case fVideo:
		setFirst(&info.VideoCodec, p.v)
	case fContainer:
		setFirst(&info.Container, p.v)
	case fAudio:
		info.Audio = appendUnique(info.Audio, p.v)
	case fHDR:
		info.HDR = appendUnique(info.HDR, p.v)
	case fTranslation:
		info.Translations = appendUnique(info.Translations, p.v)
	case fLang:
		info.AudioLangs = appendUnique(info.AudioLangs, p.v)
	case fBits:
		if info.BitDepth == 0 {
			info.BitDepth, _ = strconv.Atoi(p.v)
		}
	case f3D:
		info.ThreeD = true
	}
}

// parseBitrate normalises a bitrate stated in the title, or returns "" when
// there is none — most titles omit it, and a placeholder would be noise.
func parseBitrate(title string) string {
	m := bitrateRE.FindStringSubmatch(title)
	if m == nil {
		return ""
	}
	value := strings.Replace(m[1], ",", ".", 1)
	unit := "kbps"
	if u := strings.ToLower(m[2]); strings.HasPrefix(u, "m") || strings.HasPrefix(u, "м") {
		unit = "Mbps"
	}
	return "~" + value + " " + unit
}

// richerChannels keeps whichever layout carries more channels.
func richerChannels(have, next string) string {
	if channelSum(next) > channelSum(have) {
		return next
	}
	return have
}

func channelSum(layout string) int {
	m := channelRE.FindStringSubmatch(layout)
	if m == nil {
		return 0
	}
	front, _ := strconv.Atoi(m[1])
	lfe, _ := strconv.Atoi(m[2])
	return front + lfe
}

func setFirst(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}
