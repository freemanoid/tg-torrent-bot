package tgbot

import (
	"testing"
	"time"

	"github.com/freemanoid/tg-torrent-bot/internal/prowlarr"
)

func TestSearchCachePutGet(t *testing.T) {
	c := newSearchCache(time.Hour)
	releases := []prowlarr.Release{{Title: "first"}, {Title: "second"}}

	id := c.Put(cachedSearch{Query: "space show", Releases: releases})
	if id == "" {
		t.Fatal("Put returned empty id")
	}

	got, ok := c.Get(id)
	if !ok {
		t.Fatalf("Get(%q) = miss, want hit", id)
	}
	if got.Query != "space show" {
		t.Errorf("Query = %q, want %q", got.Query, "space show")
	}
	if len(got.Releases) != 2 || got.Releases[0].Title != "first" {
		t.Errorf("Releases = %+v, want the two cached releases", got.Releases)
	}
}

func TestSearchCacheUnknownID(t *testing.T) {
	c := newSearchCache(time.Hour)
	if _, ok := c.Get("deadbeef"); ok {
		t.Error("Get of unknown id reported a hit")
	}
}

func TestSearchCacheTTLExpiry(t *testing.T) {
	c := newSearchCache(time.Hour)
	now := time.Now()
	c.now = func() time.Time { return now }

	id := c.Put(cachedSearch{Query: "q"})

	now = now.Add(59 * time.Minute)
	if _, ok := c.Get(id); !ok {
		t.Fatal("entry expired before its TTL")
	}

	now = now.Add(2 * time.Minute)
	if _, ok := c.Get(id); ok {
		t.Fatal("entry still cached after TTL")
	}
}

func TestSearchCachePutPurgesExpired(t *testing.T) {
	c := newSearchCache(time.Hour)
	now := time.Now()
	c.now = func() time.Time { return now }

	old := c.Put(cachedSearch{Query: "old"})
	now = now.Add(2 * time.Hour)
	c.Put(cachedSearch{Query: "new"})

	c.mu.Lock()
	_, stillThere := c.entries[old]
	size := len(c.entries)
	c.mu.Unlock()
	if stillThere {
		t.Error("expired entry survived Put's purge")
	}
	if size != 1 {
		t.Errorf("cache holds %d entries, want 1", size)
	}
}

func TestSearchCacheDistinctIDs(t *testing.T) {
	c := newSearchCache(time.Hour)
	seen := make(map[string]bool)
	for range 100 {
		id := c.Put(cachedSearch{Query: "q"})
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
