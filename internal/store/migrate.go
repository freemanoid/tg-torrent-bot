package store

import (
	"database/sql"
	"fmt"
)

// migrations holds one SQL script per schema version: a database at
// user_version N has migrations[0..N-1] applied. Append-only — never edit an
// entry that has shipped.
var migrations = []string{
	`
CREATE TABLE subscriptions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  query TEXT NOT NULL,
  include TEXT NOT NULL DEFAULT '[]',
  exclude TEXT NOT NULL DEFAULT '[]',
  min_size_mb INTEGER,
  max_size_mb INTEGER,
  paused INTEGER NOT NULL DEFAULT 0,
  grabs INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  last_checked_at TEXT
);
CREATE TABLE seen (
  sub_id INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
  guid TEXT NOT NULL,
  info_hash TEXT,
  title TEXT,
  added_at TEXT NOT NULL,
  PRIMARY KEY (sub_id, guid)
);
CREATE TABLE downloads (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  hash TEXT UNIQUE,
  title TEXT,
  source TEXT,
  status TEXT NOT NULL,
  added_at TEXT NOT NULL
);
`,
	`
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`,
	// NULL means "no cutoff", which is what every subscription created before
	// this column existed keeps: those have already grabbed part of their
	// backlog, and a retroactive cutoff would strand the rest forever.
	`
ALTER TABLE subscriptions ADD COLUMN cutoff_at TEXT;
`,
}

// migrate applies any pending migrations, tracking progress in
// PRAGMA user_version. Each migration runs in its own transaction, so a
// failure leaves the database at the last fully-applied version. It returns
// the version the database was at before migrating: 0 means the file had no
// schema at all, i.e. a brand-new install rather than an upgrade.
func migrate(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return version, fmt.Errorf("database schema version %d is newer than this binary supports (%d)", version, len(migrations))
	}
	initial := version

	for ; version < len(migrations); version++ {
		tx, err := db.Begin()
		if err != nil {
			return initial, fmt.Errorf("begin migration %d: %w", version+1, err)
		}
		if _, err := tx.Exec(migrations[version]); err != nil {
			tx.Rollback()
			return initial, fmt.Errorf("apply migration %d: %w", version+1, err)
		}
		// PRAGMA does not accept bound parameters; version+1 is a trusted int.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version+1)); err != nil {
			tx.Rollback()
			return initial, fmt.Errorf("record schema version %d: %w", version+1, err)
		}
		if err := tx.Commit(); err != nil {
			return initial, fmt.Errorf("commit migration %d: %w", version+1, err)
		}
	}
	return initial, nil
}
