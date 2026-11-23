package store

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Library is a managed collection of one or more root paths
// (e.g. 动画电影) of kind movie|series|mixed.
type Library struct {
	ID        int64
	Name      string
	Kind      string
	Paths     []string
	ItemCount int
}

// ErrPathOwned is returned when a root path already belongs to another library.
var ErrPathOwned = errors.New("path already belongs to another library")

// Item is one row of the items table (movie, series, season or episode).
type Item struct {
	ID            int64
	LibraryID     int64
	Type          string
	ParentID      *int64
	SeriesID      *int64
	SeasonNum     *int
	EpisodeNum    *int
	Title         string
	OriginalTitle string
	Year          int
	Overview      string
	Rating        float64
	RuntimeMin    int
	Genres        []string
	TmdbID        int
	Path          string
	SortTitle     string
	Fingerprint   string
	CreatedAt     int64
	UpdatedAt     int64
	DateAdded     int64 // unix seconds: newest mtime of the item's own files; 0 = not backfilled yet
	HasPoster     bool
	HasBackdrop   bool
}

// MediaSource is one .strm-backed source of an item.
type MediaSource struct {
	ID              int64
	ItemID          int64
	StrmPath        string
	TargetURL       string
	Width           int
	Height          int
	VideoCodec      string
	HDR             string
	AudioSummary    string
	MediaInfoSource string
}

// Subtitle is a sidecar subtitle file registered for an item.
type Subtitle struct {
	ID     int64
	ItemID int64
	Path   string
	Lang   string
}

const itemCols = `id, library_id, type, parent_id, series_id, season_num, episode_num,
	title, original_title, year, overview, rating, runtime_min, genres, tmdb_id,
	path, sort_title, fingerprint, created_at, updated_at,
	EXISTS(SELECT 1 FROM images WHERE item_id = items.id AND kind = 'poster'),
	EXISTS(SELECT 1 FROM images WHERE item_id = items.id AND kind = 'backdrop'),
	date_added`

func scanItem(sc interface{ Scan(...any) error }) (*Item, error) {
	return scanItemX(sc)
}

// scanItemX is scanItem plus optional trailing columns scanned into extra.
func scanItemX(sc interface{ Scan(...any) error }, extra ...any) (*Item, error) {
	var it Item
	var parentID, seriesID sql.NullInt64
	var seasonNum, episodeNum sql.NullInt64
	var genres string
	var hasPoster, hasBackdrop int
	dests := []any{&it.ID, &it.LibraryID, &it.Type, &parentID, &seriesID,
		&seasonNum, &episodeNum, &it.Title, &it.OriginalTitle, &it.Year,
		&it.Overview, &it.Rating, &it.RuntimeMin, &genres, &it.TmdbID, &it.Path,
		&it.SortTitle, &it.Fingerprint, &it.CreatedAt, &it.UpdatedAt,
		&hasPoster, &hasBackdrop, &it.DateAdded}
	err := sc.Scan(append(dests, extra...)...)
	if err != nil {
		return nil, err
	}
	if parentID.Valid {
		it.ParentID = &parentID.Int64
	}
	if seriesID.Valid {
		it.SeriesID = &seriesID.Int64
	}
	if seasonNum.Valid {
		n := int(seasonNum.Int64)
		it.SeasonNum = &n
	}
	if episodeNum.Valid {
		n := int(episodeNum.Int64)
		it.EpisodeNum = &n
	}
	_ = json.Unmarshal([]byte(genres), &it.Genres)
	if it.Genres == nil {
		it.Genres = []string{}
	}
	it.HasPoster = hasPoster != 0
	it.HasBackdrop = hasBackdrop != 0
	return &it, nil
}

// ---- writes (scanner) ----

// Tx batches scanner writes; the scanner commits every <=500 items.
type Tx struct {
	tx *sql.Tx
}

// Begin starts a write transaction.
func (s *Store) Begin() (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// Commit commits the batch.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback discards the batch.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// UpsertLibrary inserts or updates a library row, returning its id.
// The path is also recorded in library_paths.
func (s *Store) UpsertLibrary(name, kind, path string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`
		INSERT INTO libraries(name, kind, path) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET name = excluded.name, kind = excluded.kind
		RETURNING id`, name, kind, path).Scan(&id)
	if err != nil {
		return 0, err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO library_paths(library_id, path) VALUES (?, ?)`, id, path)
	return id, err
}

// CountLibraries returns how many libraries exist.
func (s *Store) CountLibraries() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM libraries`).Scan(&n)
	return n, err
}

// checkPathsFree reports ErrPathOwned if any path belongs to a library
// other than excludeID (0 = no exclusion).
func (s *Store) checkPathsFree(paths []string, excludeID int64) error {
	for _, p := range paths {
		var owner int64
		err := s.db.QueryRow(`SELECT library_id FROM library_paths WHERE path = ?`, p).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if owner != excludeID {
			return fmt.Errorf("%w: %s", ErrPathOwned, p)
		}
	}
	return nil
}

// CreateLibrary inserts a new library with the given root paths.
func (s *Store) CreateLibrary(name, kind string, paths []string) (int64, error) {
	if err := s.checkPathsFree(paths, 0); err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`INSERT INTO libraries(name, kind, path) VALUES (?, ?, ?) RETURNING id`,
		name, kind, paths[0]).Scan(&id); err != nil {
		return 0, err
	}
	for _, p := range paths {
		if _, err := tx.Exec(`INSERT INTO library_paths(library_id, path) VALUES (?, ?)`, id, p); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateLibrary replaces a library's name, kind and root paths.
func (s *Store) UpdateLibrary(id int64, name, kind string, paths []string) error {
	if err := s.checkPathsFree(paths, id); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE libraries SET name = ?, kind = ?, path = ? WHERE id = ?`,
		name, kind, paths[0], id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`DELETE FROM library_paths WHERE library_id = ?`, id); err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := tx.Exec(`INSERT INTO library_paths(library_id, path) VALUES (?, ?)`, id, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteLibrary removes a library and (via FK cascade) all its items and
// their sources/images/subtitles. Files on disk are never touched.
func (s *Store) DeleteLibrary(id int64) error {
	res, err := s.db.Exec(`DELETE FROM libraries WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetItemByPath returns the item stored under path, or nil.
func (s *Store) GetItemByPath(path string) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE path = ?`, path)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// UpsertItem inserts the item or updates it in place (created_at preserved).
func (t *Tx) UpsertItem(it *Item) (int64, error) {
	genres, _ := json.Marshal(it.Genres)
	var id int64
	err := t.tx.QueryRow(`
		INSERT INTO items(library_id, type, parent_id, series_id, season_num, episode_num,
			title, original_title, year, overview, rating, runtime_min, genres,
			tmdb_id, path, sort_title, fingerprint, created_at, updated_at, date_added)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			library_id = excluded.library_id, type = excluded.type,
			parent_id = excluded.parent_id, series_id = excluded.series_id,
			season_num = excluded.season_num, episode_num = excluded.episode_num,
			title = excluded.title, original_title = excluded.original_title,
			year = excluded.year, overview = excluded.overview,
			rating = excluded.rating, runtime_min = excluded.runtime_min,
			genres = excluded.genres, tmdb_id = excluded.tmdb_id,
			sort_title = excluded.sort_title, fingerprint = excluded.fingerprint,
			updated_at = excluded.updated_at, date_added = excluded.date_added
		RETURNING id`,
		it.LibraryID, it.Type, it.ParentID, it.SeriesID, it.SeasonNum, it.EpisodeNum,
		it.Title, it.OriginalTitle, it.Year, it.Overview, it.Rating, it.RuntimeMin,
		string(genres), it.TmdbID, it.Path, it.SortTitle, it.Fingerprint,
		it.CreatedAt, it.UpdatedAt, it.DateAdded).Scan(&id)
	return id, err
}

// ReplaceSources swaps all media sources of an item.
func (t *Tx) ReplaceSources(itemID int64, sources []MediaSource) error {
	if _, err := t.tx.Exec(`DELETE FROM media_sources WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for _, ms := range sources {
		if _, err := t.tx.Exec(`
			INSERT INTO media_sources(item_id, strm_path, target_url, width, height,
				video_codec, hdr, audio_summary, mediainfo_source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			itemID, ms.StrmPath, ms.TargetURL, ms.Width, ms.Height, ms.VideoCodec,
			ms.HDR, ms.AudioSummary, ms.MediaInfoSource); err != nil {
			return err
		}
	}
	return nil
}

// ImageFile is one registered image of an item: local path plus pixel
// dimensions (0 = unknown, e.g. the header could not be parsed).
type ImageFile struct {
	Path   string
	Width  int
	Height int
}

// ReplaceImages swaps all images of an item. images maps kind -> file.
func (t *Tx) ReplaceImages(itemID int64, images map[string]ImageFile) error {
	if _, err := t.tx.Exec(`DELETE FROM images WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for kind, img := range images {
		if _, err := t.tx.Exec(`INSERT INTO images(item_id, kind, local_path, width, height) VALUES (?, ?, ?, ?, ?)`,
			itemID, kind, img.Path, img.Width, img.Height); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceSubtitles swaps all subtitles of an item.
func (t *Tx) ReplaceSubtitles(itemID int64, subs []Subtitle) error {
	if _, err := t.tx.Exec(`DELETE FROM subtitles WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for _, sub := range subs {
		if _, err := t.tx.Exec(`INSERT INTO subtitles(item_id, path, lang) VALUES (?, ?, ?)`,
			itemID, sub.Path, sub.Lang); err != nil {
			return err
		}
	}
	return nil
}

// DeleteVanished removes items of the library whose path is not in seen.
// Paths are streamed with a cursor and deleted in batches of 500.
func (s *Store) DeleteVanished(libraryID int64, seen map[string]struct{}) (int, error) {
	rows, err := s.db.Query(`SELECT id, path FROM items WHERE library_id = ?`, libraryID)
	if err != nil {
		return 0, err
	}
	var doomed []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := seen[p]; !ok {
			doomed = append(doomed, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	deleted := 0
	for len(doomed) > 0 {
		n := 500
		if len(doomed) < n {
			n = len(doomed)
		}
		batch := doomed[:n]
		doomed = doomed[n:]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		res, err := s.db.Exec(`DELETE FROM items WHERE id IN (`+ph+`)`, int64s(batch)...)
		if err != nil {
			return deleted, err
		}
		affected, _ := res.RowsAffected()
		deleted += int(affected)
	}
	return deleted, nil
}

func int64s(v []int64) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// ---- reads (API) ----

// ListLibraries returns all libraries with their root paths and
// top-level item counts.
func (s *Store) ListLibraries() ([]Library, error) {
	rows, err := s.db.Query(`
		SELECT l.id, l.name, l.kind,
			(SELECT COUNT(*) FROM items i WHERE i.library_id = l.id AND i.parent_id IS NULL)
		FROM libraries l ORDER BY l.id`)
	if err != nil {
		return nil, err
	}
	var out []Library
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Kind, &l.ItemCount); err != nil {
			rows.Close()
			return nil, err
		}
		l.Paths = []string{}
		out = append(out, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	prows, err := s.db.Query(`SELECT library_id, path FROM library_paths ORDER BY library_id, path`)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	byID := map[int64]*Library{}
	for i := range out {
		byID[out[i].ID] = &out[i]
	}
	for prows.Next() {
		var libID int64
		var p string
		if err := prows.Scan(&libID, &p); err != nil {
			return nil, err
		}
		if l, ok := byID[libID]; ok {
			l.Paths = append(l.Paths, p)
		}
	}
	return out, prows.Err()
}

// GetLibrary returns one library or nil.
func (s *Store) GetLibrary(id int64) (*Library, error) {
	libs, err := s.ListLibraries()
	if err != nil {
		return nil, err
	}
	for i := range libs {
		if libs[i].ID == id {
			return &libs[i], nil
		}
	}
	return nil, nil
}

// LibraryPaths returns all library root paths (image-serving guard).
func (s *Store) LibraryPaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM library_paths`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type cursorKey struct {
	K  string  `json:"k"`
	F  float64 `json:"f"`
	ID int64   `json:"id"`
}

func encodeCursor(ck cursorKey) string {
	b, _ := json.Marshal(ck)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(c string) (cursorKey, error) {
	var ck cursorKey
	if c == "" {
		return ck, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return ck, err
	}
	err = json.Unmarshal(b, &ck)
	return ck, err
}

// ListTopLevel returns one keyset page of a library's top-level items
// (movies or series). sort: "title" | "year" | "rating" | "added".
func (s *Store) ListTopLevel(libraryID int64, sort, cursor string, limit int) ([]*Item, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	ck, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", fmt.Errorf("bad cursor")
	}
	var where string
	var order string
	var args []any
	args = append(args, libraryID)
	switch sort {
	case "added":
		// Newest additions first; date_added is a unix-second timestamp,
		// carried in the cursor's float slot (exact for any realistic date).
		order = `date_added DESC, id DESC`
		if cursor != "" {
			where = ` AND (date_added < ? OR (date_added = ? AND id < ?))`
			args = append(args, int64(ck.F), int64(ck.F), ck.ID)
		}
	case "year":
		order = `year DESC, id ASC`
		if cursor != "" {
			where = ` AND (year < ? OR (year = ? AND id > ?))`
			args = append(args, int(ck.F), int(ck.F), ck.ID)
		}
	case "rating":
		// Plausible ratings first (bogus 10.0 and unrated 0.0 sink to the
		// bottom), then rating DESC — the cursor predicate mirrors the
		// two-bucket ordering via the cursor rating's own bucket.
		order = `(CASE WHEN rating BETWEEN 6.5 AND 9.9 THEN 0 ELSE 1 END), rating DESC, id ASC`
		if cursor != "" {
			bucket := 1
			if ck.F >= 6.5 && ck.F <= 9.9 {
				bucket = 0
			}
			where = ` AND ((CASE WHEN rating BETWEEN 6.5 AND 9.9 THEN 0 ELSE 1 END) > ?
				OR ((CASE WHEN rating BETWEEN 6.5 AND 9.9 THEN 0 ELSE 1 END) = ? AND rating < ?)
				OR ((CASE WHEN rating BETWEEN 6.5 AND 9.9 THEN 0 ELSE 1 END) = ? AND rating = ? AND id > ?))`
			args = append(args, bucket, bucket, ck.F, bucket, ck.F, ck.ID)
		}
	default:
		sort = "title"
		order = `sort_title ASC, id ASC`
		if cursor != "" {
			where = ` AND (sort_title > ? OR (sort_title = ? AND id > ?))`
			args = append(args, ck.K, ck.K, ck.ID)
		}
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items
		WHERE library_id = ? AND parent_id IS NULL`+where+` ORDER BY `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		next = encodeCursor(cursorKey{K: last.SortTitle, F: float64(max(last.Year, 0)), ID: last.ID})
		if sort == "rating" {
			next = encodeCursor(cursorKey{F: last.Rating, ID: last.ID})
		}
		if sort == "added" {
			next = encodeCursor(cursorKey{F: float64(last.DateAdded), ID: last.ID})
		}
	}
	return out, next, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// GetItem returns one item or nil.
func (s *Store) GetItem(id int64) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE id = ?`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// Children returns the direct children of an item, ordered for display
// (seasons by season_num, episodes by episode_num, else by sort_title).
func (s *Store) Children(parentID int64) ([]*Item, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items WHERE parent_id = ?
		ORDER BY COALESCE(season_num, 0), COALESCE(episode_num, 0), sort_title, id`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SourcesOf returns the media sources of an item.
func (s *Store) SourcesOf(itemID int64) ([]MediaSource, error) {
	rows, err := s.db.Query(`
		SELECT id, item_id, strm_path, target_url, width, height, video_codec, hdr,
			audio_summary, mediainfo_source
		FROM media_sources WHERE item_id = ? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaSource
	for rows.Next() {
		var ms MediaSource
		if err := rows.Scan(&ms.ID, &ms.ItemID, &ms.StrmPath, &ms.TargetURL, &ms.Width,
			&ms.Height, &ms.VideoCodec, &ms.HDR, &ms.AudioSummary, &ms.MediaInfoSource); err != nil {
			return nil, err
		}
		out = append(out, ms)
	}
	return out, rows.Err()
}

// SubtitlesOf returns the subtitles of an item.
func (s *Store) SubtitlesOf(itemID int64) ([]Subtitle, error) {
	rows, err := s.db.Query(`SELECT id, item_id, path, lang FROM subtitles WHERE item_id = ? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subtitle
	for rows.Next() {
		var sub Subtitle
		if err := rows.Scan(&sub.ID, &sub.ItemID, &sub.Path, &sub.Lang); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ImagePath returns the registered local path for an item image kind.
func (s *Store) ImagePath(itemID int64, kind string) (string, error) {
	var p string
	err := s.db.QueryRow(`SELECT local_path FROM images WHERE item_id = ? AND kind = ?`, itemID, kind).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return p, err
}

// GetSubtitle returns one subtitle row by id, or nil.
func (s *Store) GetSubtitle(id int64) (*Subtitle, error) {
	var sub Subtitle
	err := s.db.QueryRow(`SELECT id, item_id, path, lang FROM subtitles WHERE id = ?`, id).
		Scan(&sub.ID, &sub.ItemID, &sub.Path, &sub.Lang)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// DeleteItem removes one item row. Foreign-key cascades clear its
// children (seasons/episodes) and every dependent row (sources, images,
// subtitles, people, user_data); the FTS delete trigger keeps the search
// index in sync.
func (s *Store) DeleteItem(id int64) error {
	res, err := s.db.Exec(`DELETE FROM items WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Hero returns the highest-rated top-level item that has a backdrop.
func (s *Store) Hero() (*Item, error) {
	heroes, err := s.Heroes(1)
	if err != nil || len(heroes) == 0 {
		return nil, err
	}
	return heroes[0], nil
}

// Heroes returns up to limit highest-rated top-level items that have a
// backdrop (hero rotation candidates for the home page). Ratings are
// restricted to the plausible band: the library carries thousands of 0.0
// (unrated) rows and a batch of bogus 10.0 scores written by scrapers,
// both of which would otherwise poison the rotation. Within the band,
// items are first tiered by backdrop pixel width (>=1920, then >=1280,
// then the rest / unknown) so the full-screen hero prefers genuinely
// high-resolution artwork; rating decides inside a tier.
func (s *Store) Heroes(limit int) ([]*Item, error) {
	if limit <= 0 || limit > 50 {
		limit = 5
	}
	rows, err := s.db.Query(`SELECT `+itemCols+`,
			COALESCE((SELECT width FROM images WHERE item_id = items.id AND kind = 'backdrop'), 0) AS bw
		FROM items
		WHERE parent_id IS NULL
		  AND rating BETWEEN 6.5 AND 9.9
		  AND EXISTS(SELECT 1 FROM images WHERE item_id = items.id AND kind = 'backdrop')
		ORDER BY (CASE WHEN bw >= 1920 THEN 0 WHEN bw >= 1280 THEN 1 ELSE 2 END),
			rating DESC, year DESC, id ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		var bw int
		it, err := scanItemX(rows, &bw)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Recent returns the most recently added top-level items.
func (s *Store) Recent(limit int) ([]*Item, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items
		WHERE parent_id IS NULL ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Search runs an FTS5 prefix search over title/original_title, falling back
// to a LIKE scan when FTS yields nothing.
func (s *Store) Search(q string, limit int) ([]*Item, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	var terms []string
	for _, f := range strings.Fields(q) {
		f = strings.ReplaceAll(f, `"`, "")
		if f != "" {
			terms = append(terms, `"`+f+`"*`)
		}
	}
	if len(terms) > 0 {
		match := strings.Join(terms, " ")
		rows, err := s.db.Query(`SELECT `+itemCols+` FROM items
			JOIN items_fts ON items_fts.rowid = items.id
			WHERE items_fts MATCH ? ORDER BY items.id LIMIT ?`, match, limit)
		if err == nil {
			defer rows.Close()
			var out []*Item
			for rows.Next() {
				it, err := scanItem(rows)
				if err != nil {
					return nil, err
				}
				out = append(out, it)
			}
			if err := rows.Err(); err == nil && len(out) > 0 {
				return out, nil
			}
		}
	}
	like := "%" + q + "%"
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items
		WHERE title LIKE ? OR original_title LIKE ? ORDER BY id LIMIT ?`, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Stats aggregates library/type/mediainfo-source counts.
type Stats struct {
	Libraries       []Library
	TotalItems      int
	TotalMovies     int
	TotalSeries     int
	TotalEpisodes   int
	MediaInfoSource map[string]int
}

// Stats computes the aggregate counts.
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{MediaInfoSource: map[string]int{"json": 0, "filename": 0, "none": 0}}
	libs, err := s.ListLibraries()
	if err != nil {
		return nil, err
	}
	st.Libraries = libs
	rows, err := s.db.Query(`SELECT type, COUNT(*) FROM items GROUP BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var n int
		if err := rows.Scan(&typ, &n); err != nil {
			return nil, err
		}
		st.TotalItems += n
		switch typ {
		case "movie":
			st.TotalMovies = n
		case "series":
			st.TotalSeries = n
		case "episode":
			st.TotalEpisodes = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows2, err := s.db.Query(`SELECT mediainfo_source, COUNT(*) FROM media_sources GROUP BY mediainfo_source`)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var src string
		var n int
		if err := rows2.Scan(&src, &n); err != nil {
			return nil, err
		}
		st.MediaInfoSource[src] = n
	}
	return st, rows2.Err()
}

// ---- people ----

// Person is one cast/crew row of an item.
type Person struct {
	Name  string
	Role  string
	Kind  string // NFO <type> or TMDB kind, usually "Actor"
	Thumb string // image URL (or local path); may be empty
	Ord   int
}

// ReplacePeople swaps all people rows of an item (transactional variant
// used by the scanner).
func (t *Tx) ReplacePeople(itemID int64, people []Person) error {
	return replacePeople(t.tx, itemID, people)
}

// ReplacePeople swaps all people rows of an item.
func (s *Store) ReplacePeople(itemID int64, people []Person) error {
	return replacePeople(s.db, itemID, people)
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func replacePeople(x execer, itemID int64, people []Person) error {
	if _, err := x.Exec(`DELETE FROM people WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for _, p := range people {
		if _, err := x.Exec(`INSERT INTO people(item_id, name, role, kind, thumb, ord)
			VALUES (?, ?, ?, ?, ?, ?)`, itemID, p.Name, p.Role, p.Kind, p.Thumb, p.Ord); err != nil {
			return err
		}
	}
	return nil
}

// PeopleOf returns the people of an item in billing order.
func (s *Store) PeopleOf(itemID int64) ([]Person, error) {
	rows, err := s.db.Query(`SELECT name, role, kind, thumb, ord FROM people
		WHERE item_id = ? ORDER BY ord, name`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.Name, &p.Role, &p.Kind, &p.Thumb, &p.Ord); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- app settings ----

// GetSetting returns a setting value ("" when unset).
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting value; an empty value deletes the row.
func (s *Store) SetSetting(key, value string) error {
	if value == "" {
		_, err := s.db.Exec(`DELETE FROM app_settings WHERE key = ?`, key)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO app_settings(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---- user data (playback state, favorites) ----

// UserData is one user's state for one item.
type UserData struct {
	ProgressTicks int64
	Played        bool
	Favorite      bool
	LastPlayedAt  int64
}

// UserDataOf returns the user's state for an item (zero value when none).
func (s *Store) UserDataOf(itemID, userID int64) (*UserData, error) {
	var ud UserData
	var played, favorite int
	err := s.db.QueryRow(`SELECT progress_ticks, played, favorite, last_played_at
		FROM user_data WHERE item_id = ? AND user_id = ?`, itemID, userID).
		Scan(&ud.ProgressTicks, &played, &favorite, &ud.LastPlayedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &UserData{}, nil
	}
	if err != nil {
		return nil, err
	}
	ud.Played = played != 0
	ud.Favorite = favorite != 0
	return &ud, nil
}

// UpsertProgress records playback progress for an item.
func (s *Store) UpsertProgress(itemID, userID, ticks int64, played bool, lastPlayedAt int64) error {
	_, err := s.db.Exec(`INSERT INTO user_data(item_id, user_id, progress_ticks, played, favorite, last_played_at)
		VALUES (?, ?, ?, ?, 0, ?)
		ON CONFLICT(item_id, user_id) DO UPDATE SET
			progress_ticks = excluded.progress_ticks,
			played = excluded.played,
			last_played_at = excluded.last_played_at`,
		itemID, userID, ticks, boolInt(played), lastPlayedAt)
	return err
}

// ResetProgress clears progress/played for an item (favorite is kept).
func (s *Store) ResetProgress(itemID, userID int64) error {
	_, err := s.db.Exec(`INSERT INTO user_data(item_id, user_id, progress_ticks, played, favorite, last_played_at)
		VALUES (?, ?, 0, 0, 0, 0)
		ON CONFLICT(item_id, user_id) DO UPDATE SET
			progress_ticks = 0, played = 0, last_played_at = 0`, itemID, userID)
	return err
}

// SetFavorite sets the favorite flag for an item.
func (s *Store) SetFavorite(itemID, userID int64, favorite bool) error {
	_, err := s.db.Exec(`INSERT INTO user_data(item_id, user_id, progress_ticks, played, favorite, last_played_at)
		VALUES (?, ?, 0, 0, ?, 0)
		ON CONFLICT(item_id, user_id) DO UPDATE SET favorite = excluded.favorite`,
		itemID, userID, boolInt(favorite))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Favorites returns the user's favorited top-level items (movies and
// series), ordered by title for the favorites view.
func (s *Store) Favorites(userID int64) ([]*Item, error) {
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items
		JOIN user_data ud ON ud.item_id = items.id AND ud.user_id = ?
		WHERE ud.favorite = 1 AND items.parent_id IS NULL
		ORDER BY items.sort_title ASC, items.id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// LibraryThumbs returns, per library, up to three top-level item ids that
// have a backdrop (highest rated first) for the home page's library cards.
// One window-function query covers all libraries; no per-library scans.
func (s *Store) LibraryThumbs() (map[int64][]int64, error) {
	rows, err := s.db.Query(`
		SELECT library_id, item_id FROM (
			SELECT i.library_id AS library_id, i.id AS item_id,
				ROW_NUMBER() OVER (
					PARTITION BY i.library_id
					ORDER BY i.rating DESC, i.id ASC
				) AS rn
			FROM items i
			WHERE i.parent_id IS NULL
			  AND EXISTS(SELECT 1 FROM images WHERE item_id = i.id AND kind = 'backdrop')
		) WHERE rn <= 3
		ORDER BY library_id, rn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var libID, itemID int64
		if err := rows.Scan(&libID, &itemID); err != nil {
			return nil, err
		}
		out[libID] = append(out[libID], itemID)
	}
	return out, rows.Err()
}

// ContinueEntry is a partially watched movie/episode for the home page.
type ContinueEntry struct {
	Item          *Item
	ProgressTicks int64
	SeriesName    string // set for episodes
	SeasonNumber  *int   // episodes only
	EpisodeNumber *int   // episodes only
	EpisodeName   string // episodes only: the episode's own title
}

// ContinueWatching lists in-progress (started, not finished) movies and
// episodes for the user, most recently played first.
func (s *Store) ContinueWatching(userID int64, limit int) ([]ContinueEntry, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	rows, err := s.db.Query(`SELECT `+itemCols+`, ud.progress_ticks,
			COALESCE((SELECT title FROM items s WHERE s.id = items.series_id), '')
		FROM items
		JOIN user_data ud ON ud.item_id = items.id AND ud.user_id = ?
		WHERE ud.progress_ticks > 0 AND ud.played = 0
		  AND items.type IN ('movie', 'episode')
		ORDER BY ud.last_played_at DESC, items.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContinueEntry
	for rows.Next() {
		var ticks int64
		var seriesName string
		it, err := scanItemX(rows, &ticks, &seriesName)
		if err != nil {
			return nil, err
		}
		e := ContinueEntry{Item: it, ProgressTicks: ticks, SeriesName: seriesName}
		if it.Type == "episode" {
			e.SeasonNumber = it.SeasonNum
			e.EpisodeNumber = it.EpisodeNum
			e.EpisodeName = it.Title
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FirstPlayableInSeries returns the first episode (season, episode, id
// order) of a series that has at least one media source, or nil.
func (s *Store) FirstPlayableInSeries(seriesID int64) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items
		WHERE type = 'episode' AND series_id = ?
		  AND EXISTS(SELECT 1 FROM media_sources WHERE item_id = items.id)
		ORDER BY COALESCE(season_num, 0), COALESCE(episode_num, 0), id LIMIT 1`, seriesID)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// FirstPlayableInSeason returns the first episode of a season that has at
// least one media source, or nil.
func (s *Store) FirstPlayableInSeason(seasonID int64) (*Item, error) {
	row := s.db.QueryRow(`SELECT `+itemCols+` FROM items
		WHERE type = 'episode' AND parent_id = ?
		  AND EXISTS(SELECT 1 FROM media_sources WHERE item_id = items.id)
		ORDER BY COALESCE(season_num, 0), COALESCE(episode_num, 0), id LIMIT 1`, seasonID)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

// IndexEntry is the scanner's lightweight view of an existing item.
type IndexEntry struct {
	ID          int64
	Fingerprint string
	DateAdded   int64 // 0 marks a legacy row that still needs its date_added backfilled
}

// LoadIndex streams (path -> id/fingerprint) for one library so the scanner
// can resolve parents and skip unchanged items without extra queries.
func (s *Store) LoadIndex(libraryID int64) (map[string]IndexEntry, error) {
	rows, err := s.db.Query(`SELECT path, id, fingerprint, date_added FROM items WHERE library_id = ?`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]IndexEntry{}
	for rows.Next() {
		var p, fp string
		var id, added int64
		if err := rows.Scan(&p, &id, &fp, &added); err != nil {
			return nil, err
		}
		out[p] = IndexEntry{ID: id, Fingerprint: fp, DateAdded: added}
	}
	return out, rows.Err()
}
