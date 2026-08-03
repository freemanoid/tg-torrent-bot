package tgbot

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
)

// cacheTTL is how long cached search results stay usable from an inline
// keyboard; after that the user is asked to search again.
const cacheTTL = time.Hour

// cachedSearch is one search's results, kept for inline-keyboard callbacks.
// Releases are what the query's exclusions already left standing, so every
// index the keyboard carries points at a release the user can actually see.
type cachedSearch struct {
	Query    searchQuery
	Releases []prowlarr.Release
}

type cacheEntry struct {
	search  cachedSearch
	addedAt time.Time
}

// searchCache maps short random IDs to recent search results. Telegram limits
// callback data to 64 bytes, so keyboards reference results by these IDs
// instead of embedding release info. The cache is memory-only by design: a
// restart drops pending keyboards and the user simply searches again.
type searchCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time // stubbed in tests
	entries map[string]cacheEntry
}

func newSearchCache(ttl time.Duration) *searchCache {
	return &searchCache{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]cacheEntry),
	}
}

// Put stores s under a fresh ID and returns it, purging expired entries as a
// side effect so the cache cannot grow without bound.
func (c *searchCache) Put(s cachedSearch) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	for id, e := range c.entries {
		if now.Sub(e.addedAt) > c.ttl {
			delete(c.entries, id)
		}
	}

	id := newSearchID()
	for _, taken := c.entries[id]; taken; _, taken = c.entries[id] {
		id = newSearchID()
	}
	c.entries[id] = cacheEntry{search: s, addedAt: now}
	return id
}

// Get returns the cached search for id if it is present and not expired.
func (c *searchCache) Get(id string) (cachedSearch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[id]
	if !ok || c.now().Sub(e.addedAt) > c.ttl {
		return cachedSearch{}, false
	}
	return e.search, true
}

// newSearchID returns 8 random hex characters — short enough to keep callback
// data far below 64 bytes, random enough to never collide in a 1-hour window.
func newSearchID() string {
	var b [4]byte
	rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}
