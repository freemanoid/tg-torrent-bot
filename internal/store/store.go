// Package store persists bot state — subscriptions, seen releases, and
// downloads — in a single SQLite database file.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// ErrNotFound is returned when a subscription or download does not exist.
var ErrNotFound = errors.New("store: not found")

// Download statuses.
const (
	StatusActive = "active"
	StatusDone   = "done"
)

// timeFormat is how timestamps are stored in TEXT columns.
const timeFormat = time.RFC3339Nano

// Subscription is a recurring search with filters.
type Subscription struct {
	ID            int64
	Query         string
	Include       []string // required substrings/regexes (filter syntax)
	Exclude       []string // forbidden substrings/regexes
	MinSizeMB     int64    // 0 = no lower bound
	MaxSizeMB     int64    // 0 = no upper bound
	Paused        bool
	Grabs         int64
	CreatedAt     time.Time
	LastCheckedAt time.Time // zero = never checked
}

// Download is a torrent the bot handed to Transmission.
type Download struct {
	ID      int64
	Hash    string // torrent info hash from Transmission
	Title   string
	Source  string // "search" or "sub:<id>"
	Status  string // StatusActive or StatusDone
	AddedAt time.Time
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
	// schemaAtOpen is the schema version the database was at before Open
	// migrated it; 0 means the file was empty, i.e. a fresh install.
	schemaAtOpen int
}

// dsnPragmas is appended to the database path so the driver applies these
// per-connection settings to EVERY connection it opens — a pragma applied via
// Exec would be silently lost if database/sql ever replaced the connection.
const dsnPragmas = "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

// Open opens (creating if needed) the SQLite database at path and applies any
// pending schema migrations. Use ":memory:" for an in-memory database.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+dsnPragmas)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// A single connection keeps :memory: databases coherent (each new
	// connection would otherwise get its own empty database) and sidesteps
	// SQLITE_BUSY between writers; a single-user bot needs no more.
	db.SetMaxOpenConns(1)

	schemaAtOpen, err := migrate(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db, schemaAtOpen: schemaAtOpen}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// FreshDatabase reports whether Open created the schema from scratch, i.e.
// this process is the first ever to use this database. It lets callers tell a
// brand-new install apart from an upgrade of an existing one — an upgrade of a
// database that predates a feature still reports false.
func (s *Store) FreshDatabase() bool { return s.schemaAtOpen == 0 }

// --- meta ---

// Meta returns the value stored under key, or "" when the key was never set.
// An absent key is not an error: meta holds optional bookkeeping, and every
// caller starts out with nothing recorded.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read meta %s: %w", key, err)
	}
	return value, nil
}

// SetMeta stores value under key, replacing any previous value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write meta %s: %w", key, err)
	}
	return nil
}

// --- subscriptions ---

// CreateSubscription inserts sub and returns it with ID and CreatedAt set.
func (s *Store) CreateSubscription(ctx context.Context, sub Subscription) (Subscription, error) {
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = time.Now().UTC()
	}
	include, err := marshalPatterns(sub.Include)
	if err != nil {
		return Subscription{}, fmt.Errorf("encode include patterns: %w", err)
	}
	exclude, err := marshalPatterns(sub.Exclude)
	if err != nil {
		return Subscription{}, fmt.Errorf("encode exclude patterns: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO subscriptions (query, include, exclude, min_size_mb, max_size_mb, paused, grabs, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.Query, include, exclude,
		nullableInt(sub.MinSizeMB), nullableInt(sub.MaxSizeMB),
		boolToInt(sub.Paused), sub.Grabs, sub.CreatedAt.Format(timeFormat),
	)
	if err != nil {
		return Subscription{}, fmt.Errorf("create subscription: %w", err)
	}
	if sub.ID, err = res.LastInsertId(); err != nil {
		return Subscription{}, fmt.Errorf("create subscription: %w", err)
	}
	return sub, nil
}

// GetSubscription returns the subscription with the given id, or ErrNotFound.
func (s *Store) GetSubscription(ctx context.Context, id int64) (Subscription, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, query, include, exclude, COALESCE(min_size_mb, 0), COALESCE(max_size_mb, 0),
		       paused, grabs, created_at, COALESCE(last_checked_at, '')
		FROM subscriptions WHERE id = ?`, id)
	sub, err := scanSubscription(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, fmt.Errorf("subscription %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("get subscription %d: %w", id, err)
	}
	return sub, nil
}

// ListSubscriptions returns all subscriptions ordered by id.
func (s *Store) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, query, include, exclude, COALESCE(min_size_mb, 0), COALESCE(max_size_mb, 0),
		       paused, grabs, created_at, COALESCE(last_checked_at, '')
		FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list subscriptions: %w", err)
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	return subs, nil
}

// DeleteSubscription removes the subscription and (via cascade) its seen rows.
func (s *Store) DeleteSubscription(ctx context.Context, id int64) error {
	return s.mustAffectSubscription(ctx, id, "DELETE FROM subscriptions WHERE id = ?", id)
}

// SetSubscriptionPaused pauses or resumes a subscription.
func (s *Store) SetSubscriptionPaused(ctx context.Context, id int64, paused bool) error {
	return s.mustAffectSubscription(ctx, id,
		"UPDATE subscriptions SET paused = ? WHERE id = ?", boolToInt(paused), id)
}

// IncrementGrabs adds one to the subscription's grab counter.
func (s *Store) IncrementGrabs(ctx context.Context, id int64) error {
	return s.mustAffectSubscription(ctx, id,
		"UPDATE subscriptions SET grabs = grabs + 1 WHERE id = ?", id)
}

// SetLastChecked records when the subscription was last run.
func (s *Store) SetLastChecked(ctx context.Context, id int64, at time.Time) error {
	return s.mustAffectSubscription(ctx, id,
		"UPDATE subscriptions SET last_checked_at = ? WHERE id = ?", at.UTC().Format(timeFormat), id)
}

// mustAffectSubscription runs a write that targets one subscription and maps
// "no rows affected" to ErrNotFound.
func (s *Store) mustAffectSubscription(ctx context.Context, id int64, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("subscription %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("subscription %d: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("subscription %d: %w", id, ErrNotFound)
	}
	return nil
}

// --- seen releases ---

// MarkSeen records that a release guid was handled for a subscription.
// Marking the same (subID, guid) again is a no-op.
func (s *Store) MarkSeen(ctx context.Context, subID int64, guid, infoHash, title string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO seen (sub_id, guid, info_hash, title, added_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (sub_id, guid) DO NOTHING`,
		subID, guid, infoHash, title, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("mark seen (sub %d, guid %s): %w", subID, guid, err)
	}
	return nil
}

// IsSeen reports whether the release guid was already handled for a subscription.
func (s *Store) IsSeen(ctx context.Context, subID int64, guid string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM seen WHERE sub_id = ? AND guid = ?", subID, guid).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is seen (sub %d, guid %s): %w", subID, guid, err)
	}
	return true, nil
}

// --- downloads ---

// AddDownload records a torrent handed to Transmission with status active.
// Adding a hash whose row is still active is a no-op; adding a hash whose row
// is already done re-activates it (the user re-downloaded something that had
// finished before, and should get a fresh completion notification).
func (s *Store) AddDownload(ctx context.Context, hash, title, source string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO downloads (hash, title, source, status, added_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (hash) DO UPDATE SET
			title = excluded.title,
			source = excluded.source,
			status = excluded.status,
			added_at = excluded.added_at
		WHERE downloads.status = ?`,
		hash, title, source, StatusActive, time.Now().UTC().Format(timeFormat), StatusDone)
	if err != nil {
		return fmt.Errorf("add download %s: %w", hash, err)
	}
	return nil
}

// ActiveDownloads returns downloads with status active, oldest first.
func (s *Store) ActiveDownloads(ctx context.Context) ([]Download, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, hash, title, source, status, added_at
		FROM downloads WHERE status = ? ORDER BY id`, StatusActive)
	if err != nil {
		return nil, fmt.Errorf("list active downloads: %w", err)
	}
	defer rows.Close()

	dls, err := scanDownloads(rows)
	if err != nil {
		return nil, fmt.Errorf("list active downloads: %w", err)
	}
	return dls, nil
}

// FindDownloads returns the recorded downloads matching any of the given info
// hashes (case-insensitively, because Transmission's hash casing is not
// guaranteed to match what a release advertised) or any of the exact titles.
// Titles are a genuine key rather than a guess: AddDownload stores the release
// title verbatim, so anything the bot itself grabbed can be found by it even
// when the indexer published no hash. Both lists may be empty, and finding
// nothing is not an error.
func (s *Store) FindDownloads(ctx context.Context, hashes, titles []string) ([]Download, error) {
	var (
		clauses []string
		args    []any
	)
	if len(hashes) > 0 {
		clauses = append(clauses, "lower(hash) IN ("+placeholders(len(hashes))+")")
		for _, h := range hashes {
			args = append(args, strings.ToLower(h))
		}
	}
	if len(titles) > 0 {
		clauses = append(clauses, "title IN ("+placeholders(len(titles))+")")
		for _, t := range titles {
			args = append(args, t)
		}
	}
	if len(clauses) == 0 {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, hash, title, source, status, added_at
		FROM downloads WHERE `+strings.Join(clauses, " OR ")+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("find downloads: %w", err)
	}
	defer rows.Close()

	dls, err := scanDownloads(rows)
	if err != nil {
		return nil, fmt.Errorf("find downloads: %w", err)
	}
	return dls, nil
}

// RecentCompleted returns at most limit finished downloads, newest first.
// The limit is applied in SQL: done rows are never deleted, so the table grows
// for the life of the install and must not be read into memory whole.
func (s *Store) RecentCompleted(ctx context.Context, limit int) ([]Download, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, hash, title, source, status, added_at
		FROM downloads WHERE status = ? ORDER BY id DESC LIMIT ?`, StatusDone, limit)
	if err != nil {
		return nil, fmt.Errorf("list completed downloads: %w", err)
	}
	defer rows.Close()

	dls, err := scanDownloads(rows)
	if err != nil {
		return nil, fmt.Errorf("list completed downloads: %w", err)
	}
	return dls, nil
}

// CompleteDownload marks the download with the given hash as done.
// Completing an already-done download is a no-op; an unknown hash is ErrNotFound.
func (s *Store) CompleteDownload(ctx context.Context, hash string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE downloads SET status = ? WHERE hash = ?", StatusDone, hash)
	if err != nil {
		return fmt.Errorf("complete download %s: %w", hash, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete download %s: %w", hash, err)
	}
	if n == 0 {
		return fmt.Errorf("download %s: %w", hash, ErrNotFound)
	}
	return nil
}

// --- helpers ---

// scanDownloads reads a downloads result set; the column order must match the
// SELECT lists in ActiveDownloads/FindDownloads/RecentCompleted.
func scanDownloads(rows *sql.Rows) ([]Download, error) {
	var dls []Download
	for rows.Next() {
		var dl Download
		var addedAt string
		if err := rows.Scan(&dl.ID, &dl.Hash, &dl.Title, &dl.Source, &dl.Status, &addedAt); err != nil {
			return nil, err
		}
		var err error
		if dl.AddedAt, err = time.Parse(timeFormat, addedAt); err != nil {
			return nil, fmt.Errorf("parse added_at: %w", err)
		}
		dls = append(dls, dl)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dls, nil
}

// placeholders builds "?, ?, …" for an IN clause of n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// scanSubscription reads one subscriptions row; the column order must match
// the SELECT lists in GetSubscription/ListSubscriptions.
func scanSubscription(scan func(...any) error) (Subscription, error) {
	var sub Subscription
	var include, exclude, createdAt, lastChecked string
	var paused int
	err := scan(&sub.ID, &sub.Query, &include, &exclude, &sub.MinSizeMB, &sub.MaxSizeMB,
		&paused, &sub.Grabs, &createdAt, &lastChecked)
	if err != nil {
		return Subscription{}, err
	}
	sub.Paused = paused != 0
	if err := json.Unmarshal([]byte(include), &sub.Include); err != nil {
		return Subscription{}, fmt.Errorf("decode include patterns: %w", err)
	}
	if err := json.Unmarshal([]byte(exclude), &sub.Exclude); err != nil {
		return Subscription{}, fmt.Errorf("decode exclude patterns: %w", err)
	}
	if sub.CreatedAt, err = time.Parse(timeFormat, createdAt); err != nil {
		return Subscription{}, fmt.Errorf("parse created_at: %w", err)
	}
	if lastChecked != "" {
		if sub.LastCheckedAt, err = time.Parse(timeFormat, lastChecked); err != nil {
			return Subscription{}, fmt.Errorf("parse last_checked_at: %w", err)
		}
	}
	return sub, nil
}

// marshalPatterns encodes a pattern list as a JSON array, never null.
func marshalPatterns(patterns []string) (string, error) {
	if len(patterns) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(patterns)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// nullableInt maps 0 to NULL to match the nullable schema columns.
func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
