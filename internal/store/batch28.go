package store

import (
	"time"
	"unicode"

	"github.com/mozillazg/go-pinyin"
)

// ---- library display order (batch 28) ----

// SetLibraryOrder rewrites libraries.sort_order from the given id
// sequence: the first id gets position 1, the second 2, and so on.
// Ids not present in the table are ignored by the UPDATE itself.
func (s *Store) SetLibraryOrder(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE libraries SET sort_order = ? WHERE id = ?`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- pinyin initials (batch 28 fuzzy search) ----

var initialsArgs = func() pinyin.Args {
	a := pinyin.NewArgs()
	a.Style = pinyin.FirstLetter
	return a
}()

// TitleInitials returns the lowercase pinyin initials of the Han
// characters in title (钢铁侠 -> "gtx"). Non-Han characters contribute
// nothing; a title without Han characters yields "".
func TitleInitials(title string) string {
	var out []rune
	for _, r := range title {
		if !unicode.Is(unicode.Han, r) {
			continue
		}
		readings := pinyin.Pinyin(string(r), initialsArgs)
		if len(readings) == 0 || len(readings[0]) == 0 || readings[0][0] == "" {
			continue
		}
		out = append(out, []rune(readings[0][0])[0])
	}
	return string(out)
}

// startInitialsBackfill fills search_initials for rows that predate
// the column (or were written without it), in id order, 500 rows per
// batch with a pause between batches. It runs once per process on a
// cursor, uses only ordinary store queries (never called from inside
// a write transaction), and stops at the end of the table; scans keep
// new rows filled from then on.
func (s *Store) startInitialsBackfill() {
	go func() {
		// Keep out of the way of startup work (and of tests that
		// reopen the database file) before the first batch.
		time.Sleep(3 * time.Second)
		var lastID int64
		for {
			rows, err := s.db.Query(`SELECT id, title FROM items
				WHERE id > ? AND search_initials = '' ORDER BY id LIMIT 500`, lastID)
			if err != nil {
				return
			}
			type pair struct {
				id    int64
				title string
			}
			var batch []pair
			for rows.Next() {
				var p pair
				if err := rows.Scan(&p.id, &p.title); err != nil {
					rows.Close()
					return
				}
				batch = append(batch, p)
			}
			rows.Close()
			if err := rows.Err(); err != nil || len(batch) == 0 {
				return
			}
			for _, p := range batch {
				lastID = p.id
				if ini := TitleInitials(p.title); ini != "" {
					if _, err := s.db.Exec(`UPDATE items SET search_initials = ? WHERE id = ?`,
						ini, p.id); err != nil {
						return
					}
				}
			}
			if len(batch) < 500 {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// ---- mediainfo completion (batch 28 mediainfo job) ----

// PendingSource is one media source still lacking real media info
// (its info came from the filename or from nothing at all).
type PendingSource struct {
	ID        int64
	ItemID    int64
	LibraryID int64
	StrmPath  string
	TargetURL string
}

// PendingMediaInfoSources lists sources whose mediainfo_source is
// 'filename' or 'none', oldest first, capped at limit.
func (s *Store) PendingMediaInfoSources(limit int) ([]PendingSource, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := s.db.Query(`SELECT ms.id, ms.item_id, i.library_id, ms.strm_path, ms.target_url
		FROM media_sources ms JOIN items i ON i.id = ms.item_id
		WHERE ms.mediainfo_source IN ('filename', 'none')
		ORDER BY ms.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingSource
	for rows.Next() {
		var p PendingSource
		if err := rows.Scan(&p.ID, &p.ItemID, &p.LibraryID, &p.StrmPath, &p.TargetURL); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPendingMediaInfo counts the sources PendingMediaInfoSources
// would return without a limit.
func (s *Store) CountPendingMediaInfo() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM media_sources ms
		JOIN items i ON i.id = ms.item_id
		WHERE ms.mediainfo_source IN ('filename', 'none')`).Scan(&n)
	return n, err
}

// UpdateSourceMediaInfo writes probed media info back into one media
// source, marking its provenance 'probe'.
func (s *Store) UpdateSourceMediaInfo(id int64, ms MediaSource) error {
	_, err := s.db.Exec(`UPDATE media_sources SET
		width = ?, height = ?, video_codec = ?, hdr = ?, audio_summary = ?,
		mediainfo_source = 'probe', streams_json = ?, size_bytes = ?, bitrate = ?
		WHERE id = ?`,
		ms.Width, ms.Height, ms.VideoCodec, ms.HDR, ms.AudioSummary,
		ms.StreamsJSON, ms.SizeBytes, ms.Bitrate, id)
	return err
}

// ---- cover candidates (batch 28 cover generation) ----

// CoverCandidates returns up to limit top-level items of a library
// that own a backdrop, ordered for cover picking: by date_added
// (newest first) for sort "datecreated" (the default), or by rating
// for sort "rating". Mirrors LibraryArtworkItems' backdrop filter
// without its rating-band pre-selection, so the cover service can
// score a wider pool itself.
func (s *Store) CoverCandidates(libraryID int64, sortBy string, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 12
	}
	order := `date_added DESC, id DESC`
	if sortBy == "rating" {
		order = `rating DESC, date_added DESC, id DESC`
	}
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items
		WHERE library_id = ? AND parent_id IS NULL
		  AND EXISTS(SELECT 1 FROM images WHERE item_id = items.id AND kind = 'backdrop')
		ORDER BY `+order+` LIMIT ?`, libraryID, limit)
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
