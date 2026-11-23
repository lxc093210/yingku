package store

import (
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
func (s *Store) UserDataBatch(userID int64, itemIDs []int64) (map[int64]*UserData, error) {
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
