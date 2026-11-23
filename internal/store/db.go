// Package store owns the SQLite database: schema, pragmas and all queries.
// Memory discipline: every list query is keyset-paginated or cursor-streamed;
// nothing loads the whole library into memory.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed repository. A single connection is used so the
// PRAGMAs below apply to every query.
type Store struct {
	db *sql.DB
}

// Open creates (if needed) and migrates the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA cache_size=-20000", // ~20 MB page cache cap
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Migration: phase-1 databases stored a single root in libraries.path.
	// Backfill library_paths from it (idempotent; runs on every open).
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO library_paths(library_id, path)
		SELECT id, path FROM libraries WHERE path <> ''`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate library_paths: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle (used by tests).
func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS libraries (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,           -- movie | series | mixed
  path TEXT NOT NULL UNIQUE     -- legacy: primary root, mirrored from library_paths
);

-- A library owns one or more root paths; items belong to the library whose
-- root is the longest prefix of the item's path.
CREATE TABLE IF NOT EXISTS library_paths (
  library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
  path       TEXT NOT NULL,
  PRIMARY KEY (library_id, path)
);
CREATE INDEX IF NOT EXISTS idx_libpaths_path ON library_paths(path);

CREATE TABLE IF NOT EXISTS items (
  id             INTEGER PRIMARY KEY,
  library_id     INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
  type           TEXT NOT NULL, -- movie | series | season | episode
  parent_id      INTEGER REFERENCES items(id) ON DELETE CASCADE,
  series_id      INTEGER REFERENCES items(id) ON DELETE CASCADE,
  season_num     INTEGER,
  episode_num    INTEGER,
  title          TEXT NOT NULL DEFAULT '',
  original_title TEXT NOT NULL DEFAULT '',
  year           INTEGER NOT NULL DEFAULT 0,
  overview       TEXT NOT NULL DEFAULT '',
  rating         REAL NOT NULL DEFAULT 0,
  runtime_min    INTEGER NOT NULL DEFAULT 0,
  genres         TEXT NOT NULL DEFAULT '[]', -- JSON array
  tmdb_id        INTEGER NOT NULL DEFAULT 0,
  path           TEXT NOT NULL UNIQUE,
  sort_title     TEXT NOT NULL DEFAULT '',
  fingerprint    TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL DEFAULT 0, -- unix seconds
  updated_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id);
CREATE INDEX IF NOT EXISTS idx_items_series ON items(series_id, season_num, episode_num);
CREATE INDEX IF NOT EXISTS idx_items_lib_sort ON items(library_id, sort_title, id);
CREATE INDEX IF NOT EXISTS idx_items_lib_created ON items(library_id, created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS media_sources (
  id               INTEGER PRIMARY KEY,
  item_id          INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  strm_path        TEXT NOT NULL,
  target_url       TEXT NOT NULL DEFAULT '',
  width            INTEGER NOT NULL DEFAULT 0,
  height           INTEGER NOT NULL DEFAULT 0,
  video_codec      TEXT NOT NULL DEFAULT '',
  hdr              TEXT NOT NULL DEFAULT '',
  audio_summary    TEXT NOT NULL DEFAULT '',
  mediainfo_source TEXT NOT NULL DEFAULT 'none' -- json | filename | none (future: probe)
);
CREATE INDEX IF NOT EXISTS idx_ms_item ON media_sources(item_id);

CREATE TABLE IF NOT EXISTS images (
  id        INTEGER PRIMARY KEY,
  item_id   INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  kind      TEXT NOT NULL, -- poster | backdrop
  local_path TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_img_item ON images(item_id);

CREATE TABLE IF NOT EXISTS subtitles (
  id      INTEGER PRIMARY KEY,
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  path    TEXT NOT NULL,
  lang    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sub_item ON subtitles(item_id);

-- User playback state lives apart from items so rescans never touch it.
CREATE TABLE IF NOT EXISTS user_data (
  item_id        INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  user_id        INTEGER NOT NULL,
  progress_ticks INTEGER NOT NULL DEFAULT 0,
  played         INTEGER NOT NULL DEFAULT 0,
  favorite       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (item_id, user_id)
);

CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
  title, original_title, content='items', content_rowid='id'
);
CREATE TRIGGER IF NOT EXISTS items_fts_ai AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, title, original_title) VALUES (new.id, new.title, new.original_title);
END;
CREATE TRIGGER IF NOT EXISTS items_fts_ad AFTER DELETE ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, original_title)
    VALUES ('delete', old.id, old.title, old.original_title);
END;
CREATE TRIGGER IF NOT EXISTS items_fts_au AFTER UPDATE OF title, original_title ON items BEGIN
  INSERT INTO items_fts(items_fts, rowid, title, original_title)
    VALUES ('delete', old.id, old.title, old.original_title);
  INSERT INTO items_fts(rowid, title, original_title) VALUES (new.id, new.title, new.original_title);
END;
`
