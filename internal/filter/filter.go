// Package filter parses and applies subscription release filters.
//
// Filter syntax is a comma-separated token list, matched case-insensitively
// against release titles:
//
//	plain token  → required substring        (rus, 1080p, x265)
//	-token       → excluded substring        (-camrip, -720p)
//	/regex/      → required regex            (/x26[45]/)
//	-/regex/     → excluded regex            (-/cam|ts/)
//	>1gb / <30gb → size bounds               (also accepts mb)
//
// A release matches when ALL includes match, NO excludes match, and its size
// is within bounds.
package filter

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// mb is one mebibyte in bytes.
const mb int64 = 1 << 20

// Filter selects releases by title patterns and size bounds. Include and
// Exclude hold tokens in filter syntax — a plain string is a substring
// pattern, "/re/" is a regex — so the fields map directly onto the
// corresponding store.Subscription columns.
type Filter struct {
	Include   []string // every pattern must match the title
	Exclude   []string // no pattern may match the title
	MinSizeMB int64    // 0 = no lower bound
	MaxSizeMB int64    // 0 = no upper bound
	// Since rejects releases published before it, which is how a subscription
	// means "only what shows up from now on". Unlike every other field it has
	// no token in the filter syntax: Parse never sets it and String never
	// renders it. It lives here so that the engine and the /test dry run share
	// one predicate and cannot disagree about what a subscription will grab.
	Since time.Time
}

// Parse converts a filter-syntax string (the part after "|" in /sub) into a
// Filter. An empty or blank input yields the zero Filter, which matches
// everything. Empty tokens between commas are skipped; when the same size
// bound is given twice, the last one wins.
func Parse(s string) (Filter, error) {
	var f Filter
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		switch tok[0] {
		case '>', '<':
			sizeMB, err := parseSize(tok[1:])
			if err != nil {
				return Filter{}, fmt.Errorf("bad size filter %q: %w", tok, err)
			}
			if tok[0] == '>' {
				f.MinSizeMB = sizeMB
			} else {
				f.MaxSizeMB = sizeMB
			}
		case '-':
			pat := strings.TrimSpace(tok[1:])
			if pat == "" {
				return Filter{}, fmt.Errorf("empty exclude filter %q", tok)
			}
			if err := checkPattern(pat); err != nil {
				return Filter{}, err
			}
			f.Exclude = append(f.Exclude, pat)
		default:
			if err := checkPattern(tok); err != nil {
				return Filter{}, err
			}
			f.Include = append(f.Include, tok)
		}
	}
	return f, nil
}

// Match reports whether a release with the given title, size and publish date
// passes the filter. Matching is case-insensitive (including Cyrillic). A
// sizeBytes of 0 (unknown) fails any lower bound but passes an upper bound.
// Size bounds are strict, mirroring the > / < syntax.
//
// A zero published (an indexer that reported no date) passes any Since bound:
// failing it closed would leave a subscription silently grabbing nothing
// forever. Callers that need those releases held back — the engine, on a
// subscription's first tick — handle them themselves.
func (f Filter) Match(title string, sizeBytes int64, published time.Time) bool {
	if !f.Since.IsZero() && !published.IsZero() && published.Before(f.Since) {
		return false
	}
	lower := strings.ToLower(title)
	for _, pat := range f.Include {
		if !patternMatches(pat, lower) {
			return false
		}
	}
	for _, pat := range f.Exclude {
		if patternMatches(pat, lower) {
			return false
		}
	}
	if f.MinSizeMB > 0 && sizeBytes <= f.MinSizeMB*mb {
		return false
	}
	if f.MaxSizeMB > 0 && sizeBytes >= f.MaxSizeMB*mb {
		return false
	}
	return true
}

// String renders the filter back in filter syntax, suitable for showing a
// subscription to the user and for re-parsing. The zero Filter renders as "".
// Since is deliberately absent: it has no token, so rendering it would produce
// a string Parse cannot read back.
func (f Filter) String() string {
	parts := make([]string, 0, len(f.Include)+len(f.Exclude)+2)
	parts = append(parts, f.Include...)
	for _, pat := range f.Exclude {
		parts = append(parts, "-"+pat)
	}
	if f.MinSizeMB > 0 {
		parts = append(parts, ">"+formatSize(f.MinSizeMB))
	}
	if f.MaxSizeMB > 0 {
		parts = append(parts, "<"+formatSize(f.MaxSizeMB))
	}
	return strings.Join(parts, ", ")
}

// regexToken reports whether pat is a /regex/ token, returning the inner
// expression.
func regexToken(pat string) (string, bool) {
	if len(pat) >= 2 && strings.HasPrefix(pat, "/") && strings.HasSuffix(pat, "/") {
		return pat[1 : len(pat)-1], true
	}
	return "", false
}

// checkPattern validates a pattern token at parse time so Match never has to
// report errors. A token that starts with "/" but is not a complete /regex/
// is rejected loudly: tokens are split on commas before regex detection, so a
// comma inside a regex would otherwise silently degrade into two substring
// patterns that match nothing.
func checkPattern(pat string) error {
	if expr, ok := regexToken(pat); ok {
		if _, err := regexp.Compile("(?i)" + expr); err != nil {
			return fmt.Errorf("bad regex /%s/: %w", expr, err)
		}
		return nil
	}
	if strings.HasPrefix(pat, "/") {
		return fmt.Errorf("unterminated regex %q: a /regex/ must end with \"/\" and cannot contain a comma (filters split on commas — write [45] instead of {4,5})", pat)
	}
	return nil
}

// patternMatches applies one pattern token to an already-lowercased title.
// An invalid regex fails closed: it never matches (Parse rejects such tokens,
// but a Filter rebuilt by hand from stored fields might carry one).
func patternMatches(pat, lowerTitle string) bool {
	if expr, ok := regexToken(pat); ok {
		re, err := regexp.Compile("(?i)" + expr)
		if err != nil {
			return false
		}
		return re.MatchString(lowerTitle)
	}
	return strings.Contains(lowerTitle, strings.ToLower(pat))
}

// maxSizeMB caps parsed size bounds so that sizeMB*mb can never overflow
// int64 in Match (which would silently disable the bound). 2^40 MB is 1 EiB —
// far beyond any real torrent.
const maxSizeMB = int64(1) << 40

// parseSize converts the numeric part of a size token ("1.5gb", "700mb")
// into whole mebibytes.
func parseSize(s string) (int64, error) {
	lower := strings.ToLower(s)
	var mult float64
	switch {
	case strings.HasSuffix(lower, "gb"):
		mult = 1024
	case strings.HasSuffix(lower, "mb"):
		mult = 1
	default:
		return 0, fmt.Errorf("size must end in mb or gb")
	}
	num := strings.TrimSpace(lower[:len(lower)-2])
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("size is not a number: %q", num)
	}
	if math.IsNaN(v) || v <= 0 {
		return 0, fmt.Errorf("size must be positive")
	}
	sizeMB := math.Round(v * mult)
	if sizeMB > float64(maxSizeMB) {
		return 0, fmt.Errorf("size too large (max %d MB)", maxSizeMB)
	}
	return int64(sizeMB), nil
}

// formatSize renders whole mebibytes for String, using gb when exact.
func formatSize(sizeMB int64) string {
	if sizeMB%1024 == 0 {
		return strconv.FormatInt(sizeMB/1024, 10) + "gb"
	}
	return strconv.FormatInt(sizeMB, 10) + "mb"
}
