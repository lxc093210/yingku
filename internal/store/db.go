// Package store owns the SQLite database: schema, pragmas and all queries.
// Memory discipline: every list query is keyset-paginated or cursor-streamed;
// nothing loads the whole library into memory.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed repository. A single connection is used so the
// PRAGMAs below apply to every query.
type Store struct {
	db   *sql.DB
	path string
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
	s := &Store{db: db, path: path}
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
	// Migration: media_sources gained the full stream payload
	// (streams_json/size_bytes/bitrate) for rich client media info,
	// and later a resolved flag (batch 29c): Alist sources are
	// scanned from the folder NAME alone (resolved = 0, strm_path =
	// the folder) and only resolved to a concrete file at playback
	// time. The default 1 keeps every pre-existing row — all local
	// sources and old-model Alist sources whose strm_path already
	// names a file — resolved, as they are.
	for _, col := range []struct{ name, ddl string }{
		{"streams_json", `ALTER TABLE media_sources ADD COLUMN streams_json TEXT NOT NULL DEFAULT ''`},
		{"size_bytes", `ALTER TABLE media_sources ADD COLUMN size_bytes INTEGER NOT NULL DEFAULT 0`},
		{"bitrate", `ALTER TABLE media_sources ADD COLUMN bitrate INTEGER NOT NULL DEFAULT 0`},
		{"resolved", `ALTER TABLE media_sources ADD COLUMN resolved INTEGER NOT NULL DEFAULT 1`},
	} {
		has, err := columnExists(db, "media_sources", col.name)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("check media_sources.%s: %w", col.name, err)
		}
		if !has {
			if _, err := db.Exec(col.ddl); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate media_sources.%s: %w", col.name, err)
			}
		}
	}
	// Migration: user_data gained last_played_at after the first release.
	has, err := columnExists(db, "user_data", "last_played_at")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate user_data: %w", err)
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE user_data ADD COLUMN last_played_at INTEGER NOT NULL DEFAULT 0`); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate user_data last_played_at: %w", err)
		}
	}
	// Migration (batch 12): user_data.user_id was INTEGER (the single
	// implicit user, id 1) before real accounts existed; users.id is a
	// TEXT key, so the column is rebuilt as TEXT, values cast ('1'
	// stays '1' and keeps matching LegacyUserID until the first admin
	// adopts the rows). Runs after the last_played_at migration so the
	// copy below can name every column.
	uidType, err := columnType(db, "user_data", "user_id")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate user_data user_id: %w", err)
	}
	if uidType != "" && !strings.EqualFold(uidType, "TEXT") {
		for _, stmt := range []string{
			`CREATE TABLE user_data_text (
				item_id        INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
				user_id        TEXT NOT NULL,
				progress_ticks INTEGER NOT NULL DEFAULT 0,
				played         INTEGER NOT NULL DEFAULT 0,
				favorite       INTEGER NOT NULL DEFAULT 0,
				last_played_at INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (item_id, user_id)
			)`,
			`INSERT INTO user_data_text(item_id, user_id, progress_ticks, played, favorite, last_played_at)
				SELECT item_id, CAST(user_id AS TEXT), progress_ticks, played, favorite, last_played_at FROM user_data`,
			`DROP TABLE user_data`,
			`ALTER TABLE user_data_text RENAME TO user_data`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate user_data user_id to TEXT: %w", err)
			}
		}
	}
	// Migration: items gained date_added (newest source-file mtime, used
	// by the "latest additions" ordering). The companion index can only be
	// created once the column exists, so it lives here, not in the schema.
	has, err = columnExists(db, "items", "date_added")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate items: %w", err)
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE items ADD COLUMN date_added INTEGER NOT NULL DEFAULT 0`); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate items date_added: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_items_lib_added
		ON items(library_id, date_added DESC, id DESC)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate items date_added index: %w", err)
	}
	// Migration: images gained pixel dimensions (hero quality tiers).
	for _, col := range []string{"width", "height"} {
		has, err := columnExists(db, "images", col)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate images: %w", err)
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE images ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate images %s: %w", col, err)
			}
		}
	}
	// Migration (batch 28): libraries gained sort_order — the home
	// page display order, editable from the settings page. Existing
	// libraries are numbered by id once, when the column appears.
	has, err = columnExists(db, "libraries", "sort_order")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate libraries: %w", err)
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE libraries ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0`); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate libraries sort_order: %w", err)
		}
		if _, err := db.Exec(`UPDATE libraries SET sort_order = id`); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate libraries sort_order values: %w", err)
		}
	}
	// Migration (batch 29): libraries gained source_type — 'local'
	// (default; roots are local folders) or 'alist' (the single root
	// is a remote path on an Alist server).
	has, err = columnExists(db, "libraries", "source_type")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate libraries: %w", err)
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE libraries ADD COLUMN source_type TEXT NOT NULL DEFAULT 'local'`); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate libraries source_type: %w", err)
		}
	}
	// Migration (batch 28): items gained search_initials — the pinyin
	// initials of the title, used by fuzzy search. Filled by scans as
	// items are written and by a background pass for existing rows.
	has, err = columnExists(db, "items", "search_initials")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate items search_initials: %w", err)
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE items ADD COLUMN search_initials TEXT NOT NULL DEFAULT ''`); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate items search_initials: %w", err)
		}
	}
	s.startInitialsBackfill()
	return s, nil
}

// columnType returns the declared type of a column ("" when absent).
func columnType(db *sql.DB, table, column string) (string, error) {
	rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return "", err
		}
		if name == column {
			return typ, nil
		}
	}
	return "", rows.Err()
}

// columnExists reports whether table has a column of the given name.
func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DBPath returns the database file path given to Open.
func (s *Store) DBPath() string { return s.path }

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
  updated_at     INTEGER NOT NULL DEFAULT 0,
  date_added     INTEGER NOT NULL DEFAULT 0  -- unix seconds: newest mtime of the item's own files (0 = not yet backfilled)
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
  mediainfo_source TEXT NOT NULL DEFAULT 'none', -- json | filename | none | probe
  streams_json     TEXT NOT NULL DEFAULT '',
  size_bytes       INTEGER NOT NULL DEFAULT 0,
  bitrate          INTEGER NOT NULL DEFAULT 0,
  resolved         INTEGER NOT NULL DEFAULT 1  -- batch 29c: 0 = Alist folder-level source, resolved to a file at playback time
);
CREATE INDEX IF NOT EXISTS idx_ms_item ON media_sources(item_id);

CREATE TABLE IF NOT EXISTS images (
  id        INTEGER PRIMARY KEY,
  item_id   INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  kind      TEXT NOT NULL, -- poster | backdrop
  local_path TEXT NOT NULL,
  width     INTEGER NOT NULL DEFAULT 0, -- pixels, 0 = unknown
  height    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_img_item ON images(item_id);

CREATE TABLE IF NOT EXISTS subtitles (
  id      INTEGER PRIMARY KEY,
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  path    TEXT NOT NULL,
  lang    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sub_item ON subtitles(item_id);

-- Cast & crew of an item, replaced wholesale from the item's NFO on scan.
CREATE TABLE IF NOT EXISTS people (
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  name    TEXT NOT NULL DEFAULT '',
  role    TEXT NOT NULL DEFAULT '',
  kind    TEXT NOT NULL DEFAULT '',
  thumb   TEXT NOT NULL DEFAULT '',
  ord     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (item_id, ord)
);
CREATE INDEX IF NOT EXISTS idx_people_item ON people(item_id);

-- Small key/value store for application settings (e.g. the TMDB API key).
CREATE TABLE IF NOT EXISTS app_settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);

-- User playback state lives apart from items so rescans never touch it.
-- user_id is the users.id TEXT key (batch 12); pre-batch-12 databases
-- declared it INTEGER and are rebuilt by the migration in Open.
CREATE TABLE IF NOT EXISTS user_data (
  item_id        INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  user_id        TEXT NOT NULL,
  progress_ticks INTEGER NOT NULL DEFAULT 0,
  played         INTEGER NOT NULL DEFAULT 0,
  favorite       INTEGER NOT NULL DEFAULT 0,
  last_played_at INTEGER NOT NULL DEFAULT 0, -- unix seconds, 0 = never
  PRIMARY KEY (item_id, user_id)
);

-- Accounts (batch 12): one row per login-able user. password_hash is a
-- PBKDF2-HMAC-SHA256 digest string, never a plaintext password.
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY, -- 32 hex chars, same shape as server_id
  name          TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL DEFAULT '',
  is_admin      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL DEFAULT 0 -- unix seconds
);

-- Issued access tokens, one per login (device). Only the SHA-256 hex
-- of the token is stored, so a database leak does not leak sessions.
CREATE TABLE IF NOT EXISTS access_tokens (
  token_hash   TEXT PRIMARY KEY, -- SHA-256 hex of the token
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL DEFAULT 0, -- unix seconds
  last_used_at INTEGER NOT NULL DEFAULT 0  -- unix seconds, throttled writes
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
