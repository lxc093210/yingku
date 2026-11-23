package store

import (
	"database/sql"
	"errors"
	"strings"
)

// ---- Emby-surface reads ----
//
// The Emby protocol layer needs filtered, counted, offset-paginated item
// queries (ParentId / Ids / type filter / search / sort) that the web
// UI's keyset-paginated reads do not cover. They live here, next to the
// other store queries; result sets are bounded by the caller's Limit.

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ItemsFilter describes one Emby-style Items query. Exactly one scope
// applies, in priority order: IDs, then Parent (library or item).
type ItemsFilter struct {
	IDs []int64

	HasParent       bool
	ParentIsLibrary bool
	ParentLibraryID int64 // valid when HasParent && ParentIsLibrary
	ParentItemID    int64 // valid when HasParent && !ParentIsLibrary
	Recursive       bool  // flatten all descendants of the parent
	TopLevel        bool  // only with no IDs/parent scope: parent_id IS NULL

	Types  []string // internal type names (movie|series|season|episode)
	Search string   // substring match on title/original_title

	SortField string // "name" (default) or "date"
	Desc      bool
	Start     int
	Limit     int // <=0 means no limit
}

// QueryItems runs the filter and returns one page plus the total number
// of matching items before pagination.
func (s *Store) QueryItems(f ItemsFilter) ([]*Item, int, error) {
	var cte string
	var prefixArgs []any
	var where []string
	var args []any

	switch {
	case len(f.IDs) > 0:
		where = append(where, "id IN ("+placeholders(len(f.IDs))+")")
		args = append(args, int64s(f.IDs)...)
	case f.HasParent && f.ParentIsLibrary:
		where = append(where, "library_id = ?")
		args = append(args, f.ParentLibraryID)
		if !f.Recursive {
			where = append(where, "parent_id IS NULL")
		}
	case f.HasParent && f.Recursive:
		cte = `WITH RECURSIVE yk_desc(id) AS (
			SELECT id FROM items WHERE parent_id = ?
			UNION ALL
			SELECT i.id FROM items i JOIN yk_desc d ON i.parent_id = d.id) `
		prefixArgs = append(prefixArgs, f.ParentItemID)
		where = append(where, "id IN (SELECT id FROM yk_desc)")
	case f.HasParent:
		where = append(where, "parent_id = ?")
		args = append(args, f.ParentItemID)
	}
	if f.TopLevel && len(f.IDs) == 0 && !f.HasParent {
		where = append(where, "parent_id IS NULL")
	}
	if len(f.Types) > 0 {
		where = append(where, "type IN ("+placeholders(len(f.Types))+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	if f.Search != "" {
		like := "%" + f.Search + "%"
		where = append(where, "(title LIKE ? OR original_title LIKE ?)")
		args = append(args, like, like)
	}
	whereSQL := "1=1"
	if len(where) > 0 {
		whereSQL = strings.Join(where, " AND ")
	}
	allArgs := append(append([]any{}, prefixArgs...), args...)

	var total int
	if err := s.db.QueryRow(cte+`SELECT COUNT(*) FROM items WHERE `+whereSQL, allArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	dir := "ASC"
	if f.Desc {
		dir = "DESC"
	}
	col := "sort_title"
	if f.SortField == "date" {
		col = "date_added"
	}
	limit := f.Limit
	if limit <= 0 {
		limit = -1
	}
	pageArgs := append(append([]any{}, allArgs...), limit, f.Start)
	rows, err := s.db.Query(cte+`SELECT `+itemCols+` FROM items WHERE `+whereSQL+
		` ORDER BY `+col+` `+dir+`, id `+dir+` LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// SimilarItems returns top-level items similar to the given one for
// client recommendation rows: same library first, items sharing the
// first genre ranked ahead, then newest first.
func (s *Store) SimilarItems(item *Item, limit int) ([]*Item, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	genre := ""
	if len(item.Genres) > 0 {
		genre = strings.TrimSpace(item.Genres[0])
	}
	q := `SELECT ` + itemCols + ` FROM items
		WHERE parent_id IS NULL AND id != ? AND library_id = ?
		ORDER BY (CASE WHEN ? != '' AND genres LIKE ? THEN 0 ELSE 1 END),
			date_added DESC, id DESC LIMIT ?`
	rows, err := s.db.Query(q, item.ID, item.LibraryID, genre, "%"+genre+"%", limit)
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

// LibraryArtworkItem returns the id of the item whose artwork best
// represents a library on client home screens: the newest top-level
// item with a registered backdrop, falling back to one with a poster.
// Zero when the library has no artwork at all.
func (s *Store) LibraryArtworkItem(libraryID int64) (int64, error) {
	for _, kind := range []string{"backdrop", "poster"} {
		var id int64
		err := s.db.QueryRow(
			`SELECT i.id FROM items i
			 JOIN images im ON im.item_id = i.id AND im.kind = ?
			 WHERE i.library_id = ? AND i.parent_id IS NULL
			 ORDER BY i.date_added DESC, i.id DESC LIMIT 1`,
			kind, libraryID).Scan(&id)
		if err == nil {
			return id, nil
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	return 0, nil
}

// ChildCounts returns the direct-child count per parent id, for the
// parents that have at least one child.
func (s *Store) ChildCounts(parentIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(parentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT parent_id, COUNT(*) FROM items
		WHERE parent_id IN (`+placeholders(len(parentIDs))+`) GROUP BY parent_id`, int64s(parentIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var n int
		if err := rows.Scan(&pid, &n); err != nil {
			return nil, err
		}
		out[pid] = n
	}
	return out, rows.Err()
}

// UserDataBatch returns the user's state for the given items; items
// without a user_data row are absent from the map.
func (s *Store) UserDataBatch(userID string, itemIDs []int64) (map[int64]*UserData, error) {
	out := map[int64]*UserData{}
	if len(itemIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT item_id, progress_ticks, played, favorite, last_played_at
		FROM user_data WHERE user_id = ? AND item_id IN (`+placeholders(len(itemIDs))+`)`,
		append([]any{userID}, int64s(itemIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ud UserData
		var played, favorite int
		if err := rows.Scan(&id, &ud.ProgressTicks, &played, &favorite, &ud.LastPlayedAt); err != nil {
			return nil, err
		}
		ud.Played = played != 0
		ud.Favorite = favorite != 0
		out[id] = &ud
	}
	return out, rows.Err()
}

// SetPlayed sets only the played flag for an item (Emby's
// PlayedItems write surface); progress and favorite are untouched.
func (s *Store) SetPlayed(itemID int64, userID string, played bool) error {
	_, err := s.db.Exec(`INSERT INTO user_data(item_id, user_id, progress_ticks, played, favorite, last_played_at)
		VALUES (?, ?, 0, ?, 0, 0)
		ON CONFLICT(item_id, user_id) DO UPDATE SET played = excluded.played`,
		itemID, userID, boolInt(played))
	return err
}

// ResumeFilter scopes an Items/Resume query. The parent scope mirrors
// ItemsFilter's decoding: a library, or an item (a series matches its
// episodes through series_id, a season through parent_id).
type ResumeFilter struct {
	HasParent       bool
	ParentIsLibrary bool
	ParentLibraryID int64
	ParentItemID    int64
	Start           int
	Limit           int // <=0 means no limit
}

// ResumeItems returns the user's in-progress movies and episodes
// (progress recorded, not marked played), most recently played first,
// plus the total before pagination.
func (s *Store) ResumeItems(userID string, f ResumeFilter) ([]*Item, int, error) {
	where := []string{"ud.user_id = ?", "ud.progress_ticks > 0", "ud.played = 0",
		"items.type IN ('movie', 'episode')"}
	args := []any{userID}
	switch {
	case f.HasParent && f.ParentIsLibrary:
		where = append(where, "items.library_id = ?")
		args = append(args, f.ParentLibraryID)
	case f.HasParent:
		where = append(where, "(items.parent_id = ? OR items.series_id = ?)")
		args = append(args, f.ParentItemID, f.ParentItemID)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM items
		JOIN user_data ud ON ud.item_id = items.id WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = -1
	}
	pageArgs := append(append([]any{}, args...), limit, f.Start)
	rows, err := s.db.Query(`SELECT `+itemCols+` FROM items
		JOIN user_data ud ON ud.item_id = items.id WHERE `+whereSQL+`
		ORDER BY ud.last_played_at DESC, items.id DESC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// NextUpEpisodes implements Shows/NextUp: for every series the user
// has any watch activity in (an episode played or in progress), the
// next unwatched episode after the highest watched one, in
// season/episode order. Series with no activity — or fully watched —
// contribute nothing. Results are ordered by the series' most recent
// activity, newest first. onlySeriesID / onlyLibraryID (0 = no filter)
// narrow the candidate series, mirroring NextUp's ParentId parameter.
func (s *Store) NextUpEpisodes(userID string, onlySeriesID, onlyLibraryID int64, limit int) ([]*Item, error) {
	if limit <= 0 {
		limit = 25
	}
	// The anchor packs (season, episode) into one sortable integer.
	anchorSQL := `SELECT i.series_id,
			MAX(COALESCE(i.season_num, 0) * 100000 + COALESCE(i.episode_num, 0)) AS anchor,
			MAX(ud.last_played_at) AS last_act
		FROM items i
		JOIN user_data ud ON ud.item_id = i.id AND ud.user_id = ?
		WHERE i.type = 'episode' AND i.series_id IS NOT NULL
		  AND (ud.played = 1 OR ud.progress_ticks > 0)`
	anchorArgs := []any{userID}
	if onlySeriesID > 0 {
		anchorSQL += ` AND i.series_id = ?`
		anchorArgs = append(anchorArgs, onlySeriesID)
	}
	if onlyLibraryID > 0 {
		anchorSQL += ` AND i.library_id = ?`
		anchorArgs = append(anchorArgs, onlyLibraryID)
	}
	anchorSQL += ` GROUP BY i.series_id ORDER BY last_act DESC, i.series_id DESC`

	rows, err := s.db.Query(anchorSQL, anchorArgs...)
	if err != nil {
		return nil, err
	}
	type anchor struct {
		seriesID int64
		key      int64
	}
	var anchors []anchor
	for rows.Next() {
		var a anchor
		var lastAct int64
		if err := rows.Scan(&a.seriesID, &a.key, &lastAct); err != nil {
			rows.Close()
			return nil, err
		}
		anchors = append(anchors, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var out []*Item
	for _, a := range anchors {
		if len(out) >= limit {
			break
		}
		// NB: no table alias — itemCols references items.id literally.
		row := s.db.QueryRow(`SELECT `+itemCols+` FROM items
			LEFT JOIN user_data ud ON ud.item_id = items.id AND ud.user_id = ?
			WHERE items.type = 'episode' AND items.series_id = ?
			  AND COALESCE(items.season_num, 0) * 100000 + COALESCE(items.episode_num, 0) > ?
			  AND COALESCE(ud.played, 0) = 0
			ORDER BY COALESCE(items.season_num, 0), COALESCE(items.episode_num, 0), items.id
			LIMIT 1`, userID, a.seriesID, a.key)
		it, err := scanItem(row)
		if err != nil {
			// A fully watched series has no next episode: scanItem
			// surfaces that as sql.ErrNoRows — skip it, keep real
			// errors.
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// TitlesByID returns the titles of the given items (missing ids absent).
func (s *Store) TitlesByID(ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT id, title FROM items WHERE id IN (`+placeholders(len(ids))+`)`, int64s(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[id] = title
	}
	return out, rows.Err()
}
