package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openMem opens an in-memory store that is closed when the test finishes.
func openMem(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	// open + migrate twice; the second open must not fail or re-apply
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d = %v", i+1, err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("read user_version: %v", err)
		}
		if version != len(migrations) {
			t.Errorf("open #%d: user_version = %d, want %d", i+1, version, len(migrations))
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d = %v", i+1, err)
		}
	}
}

func TestOpenBadPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "no-such-dir", "bot.db")); err == nil {
		t.Fatal("Open with unwritable path succeeded, want error")
	}
}

func TestSubscriptionCRUD(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	created, err := s.CreateSubscription(ctx, Subscription{
		Query:     "space show 2026",
		Include:   []string{"rus", "1080p"},
		Exclude:   []string{"720p", "camrip"},
		MinSizeMB: 1024,
		MaxSizeMB: 30720,
	})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}
	if created.ID == 0 {
		t.Error("created subscription has zero ID")
	}
	if created.CreatedAt.IsZero() {
		t.Error("created subscription has zero CreatedAt")
	}

	got, err := s.GetSubscription(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSubscription = %v", err)
	}
	if got.Query != "space show 2026" {
		t.Errorf("Query = %q, want %q", got.Query, "space show 2026")
	}
	if len(got.Include) != 2 || got.Include[0] != "rus" || got.Include[1] != "1080p" {
		t.Errorf("Include = %v, want [rus 1080p]", got.Include)
	}
	if len(got.Exclude) != 2 || got.Exclude[0] != "720p" || got.Exclude[1] != "camrip" {
		t.Errorf("Exclude = %v, want [720p camrip]", got.Exclude)
	}
	if got.MinSizeMB != 1024 || got.MaxSizeMB != 30720 {
		t.Errorf("size bounds = %d..%d, want 1024..30720", got.MinSizeMB, got.MaxSizeMB)
	}
	if got.Paused {
		t.Error("new subscription is paused, want active")
	}
	if got.Grabs != 0 {
		t.Errorf("Grabs = %d, want 0", got.Grabs)
	}
	if !got.LastCheckedAt.IsZero() {
		t.Errorf("LastCheckedAt = %v, want zero", got.LastCheckedAt)
	}

	// second subscription with no filters / no size bounds
	second, err := s.CreateSubscription(ctx, Subscription{Query: "dune part three"})
	if err != nil {
		t.Fatalf("CreateSubscription #2 = %v", err)
	}
	gotSecond, err := s.GetSubscription(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetSubscription #2 = %v", err)
	}
	if len(gotSecond.Include) != 0 || len(gotSecond.Exclude) != 0 {
		t.Errorf("empty filters round-trip = %v / %v, want empty", gotSecond.Include, gotSecond.Exclude)
	}
	if gotSecond.MinSizeMB != 0 || gotSecond.MaxSizeMB != 0 {
		t.Errorf("unset size bounds = %d..%d, want 0..0", gotSecond.MinSizeMB, gotSecond.MaxSizeMB)
	}

	subs, err := s.ListSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListSubscriptions = %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("ListSubscriptions returned %d subs, want 2", len(subs))
	}
	if subs[0].ID != created.ID || subs[1].ID != second.ID {
		t.Errorf("ListSubscriptions order = [%d %d], want [%d %d]", subs[0].ID, subs[1].ID, created.ID, second.ID)
	}

	if err := s.DeleteSubscription(ctx, created.ID); err != nil {
		t.Fatalf("DeleteSubscription = %v", err)
	}
	if _, err := s.GetSubscription(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSubscription after delete = %v, want ErrNotFound", err)
	}
	subs, err = s.ListSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListSubscriptions after delete = %v", err)
	}
	if len(subs) != 1 {
		t.Errorf("ListSubscriptions after delete returned %d subs, want 1", len(subs))
	}
}

func TestSubscriptionNotFound(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if _, err := s.GetSubscription(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSubscription(999) = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSubscription(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteSubscription(999) = %v, want ErrNotFound", err)
	}
	if err := s.SetSubscriptionPaused(ctx, 999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetSubscriptionPaused(999) = %v, want ErrNotFound", err)
	}
	if err := s.IncrementGrabs(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("IncrementGrabs(999) = %v, want ErrNotFound", err)
	}
	if err := s.SetLastChecked(ctx, 999, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetLastChecked(999) = %v, want ErrNotFound", err)
	}
}

func TestSetSubscriptionPaused(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	sub, err := s.CreateSubscription(ctx, Subscription{Query: "q"})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}

	if err := s.SetSubscriptionPaused(ctx, sub.ID, true); err != nil {
		t.Fatalf("SetSubscriptionPaused(true) = %v", err)
	}
	got, err := s.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscription = %v", err)
	}
	if !got.Paused {
		t.Error("Paused = false after pausing, want true")
	}

	if err := s.SetSubscriptionPaused(ctx, sub.ID, false); err != nil {
		t.Fatalf("SetSubscriptionPaused(false) = %v", err)
	}
	got, err = s.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscription = %v", err)
	}
	if got.Paused {
		t.Error("Paused = true after resuming, want false")
	}
}

func TestIncrementGrabsAndLastChecked(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	sub, err := s.CreateSubscription(ctx, Subscription{Query: "q"})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.IncrementGrabs(ctx, sub.ID); err != nil {
			t.Fatalf("IncrementGrabs #%d = %v", i+1, err)
		}
	}
	checked := time.Date(2026, 7, 31, 12, 30, 0, 0, time.UTC)
	if err := s.SetLastChecked(ctx, sub.ID, checked); err != nil {
		t.Fatalf("SetLastChecked = %v", err)
	}

	got, err := s.GetSubscription(ctx, sub.ID)
	if err != nil {
		t.Fatalf("GetSubscription = %v", err)
	}
	if got.Grabs != 3 {
		t.Errorf("Grabs = %d, want 3", got.Grabs)
	}
	if !got.LastCheckedAt.Equal(checked) {
		t.Errorf("LastCheckedAt = %v, want %v", got.LastCheckedAt, checked)
	}
}

func TestSeenMarkAndCheck(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	sub, err := s.CreateSubscription(ctx, Subscription{Query: "q"})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}

	seen, err := s.IsSeen(ctx, sub.ID, "guid-1")
	if err != nil {
		t.Fatalf("IsSeen before mark = %v", err)
	}
	if seen {
		t.Error("IsSeen = true before marking, want false")
	}

	if err := s.MarkSeen(ctx, sub.ID, "guid-1", "abcdef", "Release Title"); err != nil {
		t.Fatalf("MarkSeen = %v", err)
	}
	seen, err = s.IsSeen(ctx, sub.ID, "guid-1")
	if err != nil {
		t.Fatalf("IsSeen after mark = %v", err)
	}
	if !seen {
		t.Error("IsSeen = false after marking, want true")
	}

	// duplicate insert is a no-op, not an error
	if err := s.MarkSeen(ctx, sub.ID, "guid-1", "abcdef", "Release Title"); err != nil {
		t.Fatalf("duplicate MarkSeen = %v, want nil", err)
	}

	// a different sub does not share seen state
	other, err := s.CreateSubscription(ctx, Subscription{Query: "other"})
	if err != nil {
		t.Fatalf("CreateSubscription #2 = %v", err)
	}
	seen, err = s.IsSeen(ctx, other.ID, "guid-1")
	if err != nil {
		t.Fatalf("IsSeen other sub = %v", err)
	}
	if seen {
		t.Error("IsSeen = true for a different sub, want false")
	}
}

func TestSeenSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	ctx := context.Background()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	sub, err := s.CreateSubscription(ctx, Subscription{Query: "q"})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}
	if err := s.MarkSeen(ctx, sub.ID, "guid-persist", "", ""); err != nil {
		t.Fatalf("MarkSeen = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen = %v", err)
	}
	defer s.Close()
	seen, err := s.IsSeen(ctx, sub.ID, "guid-persist")
	if err != nil {
		t.Fatalf("IsSeen after reopen = %v", err)
	}
	if !seen {
		t.Error("IsSeen = false after reopen, want true")
	}
}

func TestDeleteSubscriptionCascadesSeen(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	sub, err := s.CreateSubscription(ctx, Subscription{Query: "q"})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}
	if err := s.MarkSeen(ctx, sub.ID, "guid-1", "", ""); err != nil {
		t.Fatalf("MarkSeen = %v", err)
	}
	if err := s.DeleteSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("DeleteSubscription = %v", err)
	}

	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM seen WHERE sub_id = ?", sub.ID).Scan(&n); err != nil {
		t.Fatalf("count seen rows: %v", err)
	}
	if n != 0 {
		t.Errorf("seen rows after subscription delete = %d, want 0 (cascade)", n)
	}
}

func TestDownloadsAddListComplete(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash-1", "Some Release", "search"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.AddDownload(ctx, "hash-2", "Sub Release", "sub:7"); err != nil {
		t.Fatalf("AddDownload #2 = %v", err)
	}

	active, err := s.ActiveDownloads(ctx)
	if err != nil {
		t.Fatalf("ActiveDownloads = %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("ActiveDownloads returned %d, want 2", len(active))
	}
	first := active[0]
	if first.Hash != "hash-1" || first.Title != "Some Release" || first.Source != "search" {
		t.Errorf("first download = %+v, want hash-1/Some Release/search", first)
	}
	if first.ID == 0 {
		t.Error("download has zero ID")
	}
	if first.Status != StatusActive {
		t.Errorf("Status = %q, want %q", first.Status, StatusActive)
	}
	if first.AddedAt.IsZero() {
		t.Error("download has zero AddedAt")
	}

	if err := s.CompleteDownload(ctx, "hash-1"); err != nil {
		t.Fatalf("CompleteDownload = %v", err)
	}
	active, err = s.ActiveDownloads(ctx)
	if err != nil {
		t.Fatalf("ActiveDownloads after complete = %v", err)
	}
	if len(active) != 1 || active[0].Hash != "hash-2" {
		t.Errorf("ActiveDownloads after complete = %+v, want only hash-2", active)
	}

	// completing an already-done download is a no-op, not an error
	if err := s.CompleteDownload(ctx, "hash-1"); err != nil {
		t.Errorf("CompleteDownload of done download = %v, want nil", err)
	}
}

func TestAddDownloadDuplicateHash(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash-dup", "Title", "search"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.AddDownload(ctx, "hash-dup", "Title Again", "sub:1"); err != nil {
		t.Fatalf("duplicate AddDownload = %v, want nil", err)
	}

	active, err := s.ActiveDownloads(ctx)
	if err != nil {
		t.Fatalf("ActiveDownloads = %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("ActiveDownloads returned %d rows after duplicate add, want 1", len(active))
	}
	// re-adding a still-active hash is a no-op: the original row is preserved
	if active[0].Title != "Title" || active[0].Source != "search" {
		t.Errorf("row after duplicate add = %+v, want original Title/search preserved", active[0])
	}
}

func TestAddDownloadReactivatesDoneRow(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash-re", "First Time", "search"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.CompleteDownload(ctx, "hash-re"); err != nil {
		t.Fatalf("CompleteDownload = %v", err)
	}

	// The user re-downloads the same torrent after it finished: the row must
	// flip back to active so the watcher sends a fresh completion notification.
	if err := s.AddDownload(ctx, "hash-re", "Second Time", "sub:3"); err != nil {
		t.Fatalf("re-AddDownload = %v", err)
	}

	active, err := s.ActiveDownloads(ctx)
	if err != nil {
		t.Fatalf("ActiveDownloads = %v", err)
	}
	if len(active) != 1 || active[0].Hash != "hash-re" {
		t.Fatalf("ActiveDownloads = %+v, want the reactivated row", active)
	}
	if active[0].Status != StatusActive {
		t.Errorf("Status = %q, want %q", active[0].Status, StatusActive)
	}
	if active[0].Title != "Second Time" || active[0].Source != "sub:3" {
		t.Errorf("reactivated row = %+v, want refreshed title/source", active[0])
	}
}

func TestCompleteDownloadUnknownHash(t *testing.T) {
	s := openMem(t)

	if err := s.CompleteDownload(context.Background(), "no-such-hash"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CompleteDownload(unknown) = %v, want ErrNotFound", err)
	}
}

func TestGetDownload(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "AABBCC", "Space Show S01E01", "sub:1"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}

	// Transmission's casing need not match what was stored.
	dl, err := s.GetDownload(ctx, "aabbcc")
	if err != nil {
		t.Fatalf("GetDownload = %v", err)
	}
	if dl.Title != "Space Show S01E01" || dl.Source != "sub:1" || dl.Status != StatusActive {
		t.Errorf("GetDownload = %+v, want the stored row", dl)
	}

	// A cancelled row is still readable: the delete confirmation has to be able
	// to name what a stale button points at.
	if err := s.CancelDownload(ctx, "AABBCC"); err != nil {
		t.Fatalf("CancelDownload = %v", err)
	}
	if dl, err = s.GetDownload(ctx, "AABBCC"); err != nil || dl.Status != StatusCancelled {
		t.Errorf("GetDownload after cancel = (%+v, %v), want the cancelled row", dl, err)
	}
}

func TestGetDownloadUnknownHash(t *testing.T) {
	s := openMem(t)

	if _, err := s.GetDownload(context.Background(), "no-such-hash"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDownload(unknown) = %v, want ErrNotFound", err)
	}
}

func TestFindDownloads(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "AABBCC", "Active Release", "search"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.AddDownload(ctx, "DDEEFF", "Done Release", "sub:2"); err != nil {
		t.Fatalf("AddDownload #2 = %v", err)
	}
	if err := s.CompleteDownload(ctx, "DDEEFF"); err != nil {
		t.Fatalf("CompleteDownload = %v", err)
	}

	tests := []struct {
		name   string
		hashes []string
		titles []string
		want   map[string]string // hash -> status
	}{
		{
			name:   "by hash",
			hashes: []string{"AABBCC"},
			want:   map[string]string{"AABBCC": StatusActive},
		},
		{
			// Transmission's hash casing is not guaranteed to match what a
			// release advertised, so the lookup must ignore it.
			name:   "by hash, different case",
			hashes: []string{"aabbcc"},
			want:   map[string]string{"AABBCC": StatusActive},
		},
		{
			name:   "by title",
			titles: []string{"Done Release"},
			want:   map[string]string{"DDEEFF": StatusDone},
		},
		{
			name:   "hash and title together",
			hashes: []string{"aabbcc"},
			titles: []string{"Done Release"},
			want:   map[string]string{"AABBCC": StatusActive, "DDEEFF": StatusDone},
		},
		{
			name:   "no match",
			hashes: []string{"nope"},
			titles: []string{"Never Grabbed"},
			want:   map[string]string{},
		},
		{
			name: "both lists empty",
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, err := s.FindDownloads(ctx, tt.hashes, tt.titles)
			if err != nil {
				t.Fatalf("FindDownloads = %v", err)
			}
			got := make(map[string]string, len(found))
			for _, dl := range found {
				got[dl.Hash] = dl.Status
			}
			if len(got) != len(tt.want) {
				t.Fatalf("FindDownloads returned %+v, want %v", found, tt.want)
			}
			for hash, status := range tt.want {
				if got[hash] != status {
					t.Errorf("status of %s = %q, want %q", hash, got[hash], status)
				}
			}
		})
	}
}

func TestFindDownloadsPopulatesRow(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash-full", "Full Row", "sub:9"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}

	found, err := s.FindDownloads(ctx, []string{"hash-full"}, nil)
	if err != nil {
		t.Fatalf("FindDownloads = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("FindDownloads returned %d rows, want 1", len(found))
	}
	dl := found[0]
	if dl.ID == 0 || dl.Title != "Full Row" || dl.Source != "sub:9" || dl.Status != StatusActive {
		t.Errorf("row = %+v, want fully populated", dl)
	}
	if dl.AddedAt.IsZero() {
		t.Error("row has zero AddedAt")
	}
}

func TestRecentCompleted(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	for _, hash := range []string{"h1", "h2", "h3"} {
		if err := s.AddDownload(ctx, hash, "Title "+hash, "search"); err != nil {
			t.Fatalf("AddDownload %s = %v", hash, err)
		}
		if err := s.CompleteDownload(ctx, hash); err != nil {
			t.Fatalf("CompleteDownload %s = %v", hash, err)
		}
	}
	// An active download must never show up in the completed history.
	if err := s.AddDownload(ctx, "h4", "Still Going", "search"); err != nil {
		t.Fatalf("AddDownload h4 = %v", err)
	}

	done, err := s.RecentCompleted(ctx, 10)
	if err != nil {
		t.Fatalf("RecentCompleted = %v", err)
	}
	if len(done) != 3 {
		t.Fatalf("RecentCompleted returned %d rows, want 3", len(done))
	}
	// newest first
	if done[0].Hash != "h3" || done[1].Hash != "h2" || done[2].Hash != "h1" {
		t.Errorf("RecentCompleted order = %s/%s/%s, want h3/h2/h1", done[0].Hash, done[1].Hash, done[2].Hash)
	}
	if done[0].Title != "Title h3" || done[0].Status != StatusDone || done[0].AddedAt.IsZero() {
		t.Errorf("newest row = %+v, want fully populated done row", done[0])
	}
}

func TestRecentCompletedLimit(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	for i := range 5 {
		hash := fmt.Sprintf("hash-%d", i)
		if err := s.AddDownload(ctx, hash, "Title", "search"); err != nil {
			t.Fatalf("AddDownload = %v", err)
		}
		if err := s.CompleteDownload(ctx, hash); err != nil {
			t.Fatalf("CompleteDownload = %v", err)
		}
	}

	done, err := s.RecentCompleted(ctx, 2)
	if err != nil {
		t.Fatalf("RecentCompleted = %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("RecentCompleted(2) returned %d rows, want 2", len(done))
	}
	if done[0].Hash != "hash-4" || done[1].Hash != "hash-3" {
		t.Errorf("RecentCompleted(2) = %s/%s, want the two newest", done[0].Hash, done[1].Hash)
	}

	// A non-positive limit asks for nothing and must not fall through to "all".
	if done, err = s.RecentCompleted(ctx, 0); err != nil || len(done) != 0 {
		t.Errorf("RecentCompleted(0) = (%d rows, %v), want (0, nil)", len(done), err)
	}
}

func TestRecentCompletedEmpty(t *testing.T) {
	done, err := openMem(t).RecentCompleted(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentCompleted = %v", err)
	}
	if len(done) != 0 {
		t.Errorf("RecentCompleted on empty store = %+v, want none", done)
	}
}

func TestOpenAppliesPragmasViaDSN(t *testing.T) {
	// The pragmas ride in the DSN so the driver applies them to every
	// connection it ever opens, not just the first one.
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer s.Close()

	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var busy int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
	var journal string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bump user_version: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}

	_, err = Open(path)
	if err == nil {
		t.Fatal("Open succeeded on a database from the future, want error")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error %q should say the schema is newer than this binary", err)
	}
}

func TestConcurrentAccess(t *testing.T) {
	// The engine, watcher, and Telegram handlers share one *Store across
	// goroutines in production; hammer it from several at once.
	path := filepath.Join(t.TempDir(), "bot.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	sub, err := s.CreateSubscription(ctx, Subscription{Query: "q"})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}

	const iterations = 50
	var wg sync.WaitGroup
	errs := make(chan error, 3*iterations)
	wg.Add(3)
	go func() { // subscription engine: dedupe bookkeeping
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			guid := fmt.Sprintf("guid-%d", i)
			if err := s.MarkSeen(ctx, sub.ID, guid, "", "t"); err != nil {
				errs <- err
			}
			if _, err := s.IsSeen(ctx, sub.ID, guid); err != nil {
				errs <- err
			}
		}
	}()
	go func() { // telegram handlers: recording downloads
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := s.AddDownload(ctx, fmt.Sprintf("hash-%d", i), "t", "search"); err != nil {
				errs <- err
			}
		}
	}()
	go func() { // completion watcher: polling and closing out
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := s.ActiveDownloads(ctx); err != nil {
				errs <- err
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent access error: %v", err)
	}

	seen, err := s.IsSeen(ctx, sub.ID, "guid-0")
	if err != nil || !seen {
		t.Errorf("IsSeen after concurrent writes = (%v, %v), want (true, nil)", seen, err)
	}
	active, err := s.ActiveDownloads(ctx)
	if err != nil || len(active) != iterations {
		t.Errorf("ActiveDownloads after concurrent writes = (%d rows, %v), want (%d, nil)", len(active), err, iterations)
	}
}

// --- meta ---

func TestMetaRoundTrip(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	// an unset key is not an error, it is simply empty
	got, err := s.Meta(ctx, "announced_version")
	if err != nil || got != "" {
		t.Fatalf("Meta(unset) = (%q, %v), want (\"\", nil)", got, err)
	}

	if err := s.SetMeta(ctx, "announced_version", "v1.2.0"); err != nil {
		t.Fatalf("SetMeta = %v", err)
	}
	if got, err = s.Meta(ctx, "announced_version"); err != nil || got != "v1.2.0" {
		t.Fatalf("Meta after set = (%q, %v), want (\"v1.2.0\", nil)", got, err)
	}

	// a second write replaces rather than conflicting on the primary key
	if err := s.SetMeta(ctx, "announced_version", "v1.3.0"); err != nil {
		t.Fatalf("SetMeta overwrite = %v", err)
	}
	if got, err = s.Meta(ctx, "announced_version"); err != nil || got != "v1.3.0" {
		t.Fatalf("Meta after overwrite = (%q, %v), want (\"v1.3.0\", nil)", got, err)
	}
}

func TestMetaKeysAreIndependent(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.SetMeta(ctx, "a", "1"); err != nil {
		t.Fatalf("SetMeta(a) = %v", err)
	}
	if err := s.SetMeta(ctx, "b", "2"); err != nil {
		t.Fatalf("SetMeta(b) = %v", err)
	}
	for key, want := range map[string]string{"a": "1", "b": "2"} {
		got, err := s.Meta(ctx, key)
		if err != nil || got != want {
			t.Errorf("Meta(%q) = (%q, %v), want (%q, nil)", key, got, err, want)
		}
	}
}

func TestMetaFailsOnClosedStore(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	s.Close()
	ctx := context.Background()

	if _, err := s.Meta(ctx, "k"); err == nil {
		t.Error("Meta on a closed store = nil error, want failure")
	}
	if err := s.SetMeta(ctx, "k", "v"); err == nil {
		t.Error("SetMeta on a closed store = nil error, want failure")
	}
}

func TestFreshDatabaseOnlyOnFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open #1 = %v", err)
	}
	if !first.FreshDatabase() {
		t.Error("FreshDatabase on a new file = false, want true")
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("Open #2 = %v", err)
	}
	defer second.Close()
	if second.FreshDatabase() {
		t.Error("FreshDatabase on an existing database = true, want false")
	}
}

// A database written by an older build — one that predates a migration — is an
// upgrade, not a fresh install, even though the newest tables are missing.
func TestFreshDatabaseFalseForPartiallyMigratedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	db, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatalf("apply migration 1: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer s.Close()
	if s.FreshDatabase() {
		t.Error("FreshDatabase on a pre-existing v1 database = true, want false")
	}
	// the pending migration still ran
	if err := s.SetMeta(context.Background(), "k", "v"); err != nil {
		t.Errorf("SetMeta after upgrade = %v, want the meta table to exist", err)
	}
}

func TestSubscriptionCutoffRoundTrip(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	cutoff := time.Date(2026, 8, 3, 12, 30, 0, 0, time.UTC)
	created, err := s.CreateSubscription(ctx, Subscription{Query: "space show", CutoffAt: cutoff})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}
	if !created.CutoffAt.Equal(cutoff) {
		t.Errorf("created.CutoffAt = %v, want %v", created.CutoffAt, cutoff)
	}

	got, err := s.GetSubscription(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSubscription = %v", err)
	}
	if !got.CutoffAt.Equal(cutoff) {
		t.Errorf("GetSubscription CutoffAt = %v, want %v", got.CutoffAt, cutoff)
	}

	list, err := s.ListSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListSubscriptions = %v", err)
	}
	if len(list) != 1 || !list[0].CutoffAt.Equal(cutoff) {
		t.Errorf("ListSubscriptions CutoffAt = %v, want %v", list, cutoff)
	}
}

func TestSubscriptionWithoutCutoffStaysZero(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	created, err := s.CreateSubscription(ctx, Subscription{Query: "space show"})
	if err != nil {
		t.Fatalf("CreateSubscription = %v", err)
	}
	got, err := s.GetSubscription(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSubscription = %v", err)
	}
	if !got.CutoffAt.IsZero() {
		t.Errorf("CutoffAt = %v, want zero (no cutoff)", got.CutoffAt)
	}
}

// Subscriptions that predate the cutoff column must keep grabbing everything
// they match: a retroactive cutoff would permanently strand older releases
// that maxGrabsPerTick had deferred to a later tick.
func TestCutoffZeroForSubscriptionsPredatingMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")

	db, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		t.Fatalf("sql.Open = %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("apply migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO subscriptions (query, created_at) VALUES ('old sub', ?)`,
		time.Now().UTC().Add(-90*24*time.Hour).Format(timeFormat)); err != nil {
		t.Fatalf("insert legacy subscription: %v", err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	defer s.Close()

	subs, err := s.ListSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("ListSubscriptions = %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d subscriptions, want 1", len(subs))
	}
	if !subs[0].CutoffAt.IsZero() {
		t.Errorf("legacy subscription CutoffAt = %v, want zero", subs[0].CutoffAt)
	}
}

func TestCancelDownload(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash1", "Space Show S01E01", "sub:1"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.CancelDownload(ctx, "hash1"); err != nil {
		t.Fatalf("CancelDownload = %v", err)
	}

	active, err := s.ActiveDownloads(ctx)
	if err != nil {
		t.Fatalf("ActiveDownloads = %v", err)
	}
	if len(active) != 0 {
		t.Errorf("ActiveDownloads = %v, want none after cancel", active)
	}
	done, err := s.RecentCompleted(ctx, 10)
	if err != nil {
		t.Fatalf("RecentCompleted = %v", err)
	}
	if len(done) != 0 {
		t.Errorf("RecentCompleted = %v, want none after cancel", done)
	}

	found, err := s.FindDownloads(ctx, []string{"hash1"}, nil)
	if err != nil {
		t.Fatalf("FindDownloads = %v", err)
	}
	if len(found) != 1 || found[0].Status != StatusCancelled {
		t.Errorf("FindDownloads = %v, want one row with status %q", found, StatusCancelled)
	}
}

func TestCancelDownloadAfterCompletion(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash1", "Space Show S01E01", "sub:1"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.CompleteDownload(ctx, "hash1"); err != nil {
		t.Fatalf("CompleteDownload = %v", err)
	}
	if err := s.CancelDownload(ctx, "hash1"); err != nil {
		t.Fatalf("CancelDownload after completion = %v", err)
	}
	done, err := s.RecentCompleted(ctx, 10)
	if err != nil {
		t.Fatalf("RecentCompleted = %v", err)
	}
	if len(done) != 0 {
		t.Errorf("RecentCompleted = %v, want none after cancelling a finished download", done)
	}
}

func TestCancelDownloadUnknownHash(t *testing.T) {
	s := openMem(t)
	if err := s.CancelDownload(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelDownload(unknown) = %v, want ErrNotFound", err)
	}
}

// A release the user cancelled must still be re-downloadable by hand, with a
// fresh completion notification — same as re-downloading a finished one.
func TestAddDownloadReactivatesCancelledRow(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash1", "Space Show S01E01", "sub:1"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.CancelDownload(ctx, "hash1"); err != nil {
		t.Fatalf("CancelDownload = %v", err)
	}
	if err := s.AddDownload(ctx, "hash1", "Space Show S01E01", "search"); err != nil {
		t.Fatalf("AddDownload after cancel = %v", err)
	}

	active, err := s.ActiveDownloads(ctx)
	if err != nil {
		t.Fatalf("ActiveDownloads = %v", err)
	}
	if len(active) != 1 || active[0].Status != StatusActive {
		t.Errorf("ActiveDownloads = %v, want the cancelled row re-activated", active)
	}
}

// The watcher can already hold a download in its cycle when the user rejects
// it; finishing it afterwards must not put it back into the history.
func TestCompleteDownloadWillNotResurrectCancelled(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()

	if err := s.AddDownload(ctx, "hash1", "Space Show S01E01", "sub:1"); err != nil {
		t.Fatalf("AddDownload = %v", err)
	}
	if err := s.CancelDownload(ctx, "hash1"); err != nil {
		t.Fatalf("CancelDownload = %v", err)
	}
	if err := s.CompleteDownload(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CompleteDownload(cancelled) = %v, want ErrNotFound", err)
	}
	found, err := s.FindDownloads(ctx, []string{"hash1"}, nil)
	if err != nil {
		t.Fatalf("FindDownloads = %v", err)
	}
	if len(found) != 1 || found[0].Status != StatusCancelled {
		t.Errorf("FindDownloads = %v, want the row still cancelled", found)
	}
}
