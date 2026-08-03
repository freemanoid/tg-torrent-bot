package tgbot

import (
	"slices"
	"strings"
	"testing"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
)

func TestParseSearchQuery(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		terms   string
		exclude []string
	}{
		{
			name:  "plain query is untouched",
			raw:   "space show 2026 1080p",
			terms: "space show 2026 1080p",
		},
		{
			name:    "leading minus excludes and never reaches prowlarr",
			raw:     "формула 1 2026 2160p 10 rus -AV1",
			terms:   "формула 1 2026 2160p 10 rus",
			exclude: []string{"AV1"},
		},
		{
			name:    "several exclusions",
			raw:     "space show -camrip -720p",
			terms:   "space show",
			exclude: []string{"camrip", "720p"},
		},
		{
			name:    "regex exclusion",
			raw:     "space show -/av1|vp9/",
			terms:   "space show",
			exclude: []string{"/av1|vp9/"},
		},
		{
			name:  "a hyphen inside a word is part of the term",
			raw:   "space-show WEB-DL x264",
			terms: "space-show WEB-DL x264",
		},
		{
			name:    "surrounding whitespace and blank runs collapse",
			raw:     "  space   show   -AV1  ",
			terms:   "space show",
			exclude: []string{"AV1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := parseSearchQuery(tt.raw)
			if err != nil {
				t.Fatalf("parseSearchQuery(%q): %v", tt.raw, err)
			}
			if q.Raw != strings.TrimSpace(tt.raw) {
				t.Errorf("Raw = %q, want the text as typed (%q)", q.Raw, strings.TrimSpace(tt.raw))
			}
			if q.Terms != tt.terms {
				t.Errorf("Terms = %q, want %q", q.Terms, tt.terms)
			}
			if !slices.Equal(q.Exclude, tt.exclude) {
				t.Errorf("Exclude = %v, want %v", q.Exclude, tt.exclude)
			}
		})
	}
}

func TestParseSearchQueryErrors(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "nothing left to search for", raw: "-AV1 -720p", want: "at least one word"},
		{name: "bare minus", raw: "space show -", want: "empty exclude"},
		{name: "unterminated regex", raw: "space show -/av1", want: "unterminated regex"},
		{name: "comma inside an exclusion", raw: "space show -AV1,H265", want: "not commas"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseSearchQuery(tt.raw); err == nil {
				t.Fatalf("parseSearchQuery(%q) succeeded, want an error", tt.raw)
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestSearchQueryKeep(t *testing.T) {
	releases := []prowlarr.Release{
		{Title: "Формула 1. S2026. Этап 10. [WEB-DL, AV1, 2160p] RUS"},
		{Title: "Формула 1. S2026. Этап 10. [WEB-DL, H.265, 2160p] RUS"},
	}

	q, err := parseSearchQuery("формула 1 2026 -av1")
	if err != nil {
		t.Fatalf("parseSearchQuery: %v", err)
	}
	kept := q.keep(releases)

	if len(kept) != 1 || kept[0].Title != releases[1].Title {
		t.Fatalf("kept %v, want only the non-AV1 release", kept)
	}
	// Filtering must not disturb the slice it was handed: the caller still holds
	// it, and in the engine's case the same results feed other work.
	if releases[0].Title != "Формула 1. S2026. Этап 10. [WEB-DL, AV1, 2160p] RUS" {
		t.Errorf("keep overwrote the input slice: %q", releases[0].Title)
	}
}

func TestSearchQueryKeepWithoutExclusionsIsIdentity(t *testing.T) {
	releases := nReleases(3)
	q, err := parseSearchQuery("space show")
	if err != nil {
		t.Fatalf("parseSearchQuery: %v", err)
	}
	if kept := q.keep(releases); len(kept) != 3 {
		t.Errorf("kept %d of 3 releases, want all of them", len(kept))
	}
}
