package tgbot

import (
	"errors"
	"fmt"
	"strings"

	"github.com/freemanoid/tg-torrent-bot/internal/filter"
	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
)

// searchQuery is one typed search split into the words Prowlarr is asked for
// and the exclusions this bot applies to the answer itself.
//
// Exclusions are handled here rather than passed through because Prowlarr fans
// a query out to every configured indexer, and each tracker reads query syntax
// its own way: one honours a leading "-", the next takes it as a literal, a
// third drops it. Filtering the results locally is the only way "-AV1" means
// the same thing whatever answered.
type searchQuery struct {
	Raw     string   // exactly what the user typed — what every message shows
	Terms   string   // the words sent to Prowlarr, exclusions removed
	Exclude []string // filter-syntax patterns no result may match
}

// parseSearchQuery splits a plain-text search on whitespace and pulls out the
// tokens that start with "-", which exclude a substring (-AV1) or a regex
// (-/av1|vp9/) from the results — the same tokens /sub takes after the "|",
// minus the size bounds, which have no meaning in a one-off search.
//
// Only a token whose first character is "-" is an exclusion, so "WEB-DL" and
// "space-show" stay ordinary search words.
func parseSearchQuery(raw string) (searchQuery, error) {
	q := searchQuery{Raw: strings.TrimSpace(raw)}
	fields := strings.Fields(q.Raw)
	terms := make([]string, 0, len(fields))
	for _, tok := range fields {
		if !strings.HasPrefix(tok, "-") {
			terms = append(terms, tok)
			continue
		}
		// One token at a time through the filter parser: it owns what a pattern
		// may look like, and a search must not grow a second opinion about it.
		f, err := filter.Parse(tok)
		if err != nil {
			return searchQuery{}, err
		}
		// Parse splits on commas, which a search does not: "-AV1,H265" would
		// come back as one exclusion plus an include this function has nowhere
		// to put, and half the token would vanish without a word.
		if len(f.Include) > 0 || f.MinSizeMB > 0 || f.MaxSizeMB > 0 {
			return searchQuery{}, fmt.Errorf(
				"bad exclusion %q: a search separates words with spaces, not commas — write \"-AV1 -H265\"", tok)
		}
		q.Exclude = append(q.Exclude, f.Exclude...)
	}
	if len(terms) == 0 {
		return searchQuery{}, errors.New("a search needs at least one word to look for, not just exclusions")
	}
	q.Terms = strings.Join(terms, " ")
	return q, nil
}

// keep returns the releases that survive the query's exclusions, in order. The
// input slice is left alone: filtering in place would rewrite results the
// caller still holds.
func (q searchQuery) keep(releases []prowlarr.Release) []prowlarr.Release {
	if len(q.Exclude) == 0 {
		return releases
	}
	f := filter.Filter{Exclude: q.Exclude}
	kept := make([]prowlarr.Release, 0, len(releases))
	for _, r := range releases {
		if f.Match(r.Title, r.Size, r.PublishDate) {
			kept = append(kept, r)
		}
	}
	return kept
}

// excludeList renders the exclusions the way the user typed them, for the
// messages that have to explain what was removed.
func (q searchQuery) excludeList() string {
	parts := make([]string, 0, len(q.Exclude))
	for _, pat := range q.Exclude {
		parts = append(parts, "-"+pat)
	}
	return strings.Join(parts, " ")
}
