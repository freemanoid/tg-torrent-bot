package store

import (
	"context"
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
