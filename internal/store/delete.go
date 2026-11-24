package store

import (
	"database/sql"
	"sort"
	"strings"
)

// Deep deletion (batch 28 data hygiene). Every item-removal path —
// scan pruning, the item delete endpoint, library deletion, and the
// scanner's version-merge cleanup — funnels through deleteItemsDeep,
// which removes an item's whole subtree (series → seasons →
// episodes) and every dependent row EXPLICITLY, in one transaction:
//
//   - media_sources, images, subtitles, people, user_data rows of
//     every removed item;
//   - the items rows themselves, children before parents.
//
// The schema also declares ON DELETE CASCADE for all of these and
// the FTS delete trigger keeps items_fts in sync; the explicit
// deletes make the outcome independent of cascade enforcement, so
// no path can silently leave rows behind.

// itemDepths expands roots to their full subtrees via parent_id,
// returning each item's depth (roots are 0). Unknown ids are absent.
func (s *Store) itemDepths(roots []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(roots) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(roots)), ",")
	rows, err := s.db.Query(`WITH RECURSIVE sub(id, depth) AS (
			SELECT id, 0 FROM items WHERE id IN (`+ph+`)
			UNION ALL
			SELECT i.id, sub.depth + 1 FROM items i JOIN sub ON i.parent_id = sub.id
		) SELECT id, depth FROM sub`, int64s(roots)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var depth int
		if err := rows.Scan(&id, &depth); err != nil {
			return nil, err
		}
		if old, ok := out[id]; !ok || depth > old {
			out[id] = depth
		}
	}
	return out, rows.Err()
}

// deleteItemsDeep removes the subtrees rooted at roots and returns
// every removed item id (roots included, descendants included).
func (s *Store) deleteItemsDeep(roots []int64) ([]int64, error) {
	depths, err := s.itemDepths(roots)
	if err != nil {
		return nil, err
	}
	if len(depths) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(depths))
	for id := range depths {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	execIn := func(query string, chunk []int64) error {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		_, err := tx.Exec(strings.Replace(query, "{PH}", ph, 1), int64s(chunk)...)
		return err
	}
	// Dependent rows first, in chunks (SQLite variable limit).
	for _, table := range []string{"media_sources", "images", "subtitles", "people", "user_data"} {
		for _, chunk := range chunkIDs(ids, 500) {
			if err := execIn(`DELETE FROM `+table+` WHERE item_id IN ({PH})`, chunk); err != nil {
				return nil, err
			}
		}
	}
	// Items, deepest first so children never outlive their parent
	// even without cascade enforcement.
	byDepth := map[int][]int64{}
	maxDepth := 0
	for id, d := range depths {
		byDepth[d] = append(byDepth[d], id)
		if d > maxDepth {
			maxDepth = d
		}
	}
	for d := maxDepth; d >= 0; d-- {
		for _, chunk := range chunkIDs(byDepth[d], 500) {
			if err := execIn(`DELETE FROM items WHERE id IN ({PH})`, chunk); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func chunkIDs(ids []int64, size int) [][]int64 {
	var out [][]int64
	for len(ids) > 0 {
		n := size
		if len(ids) < n {
			n = len(ids)
		}
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}

// DeleteItemDeep removes one item, its whole subtree and every
// dependent row, returning all removed item ids (empty when the
// item does not exist). The API layer uses the ids to purge the
// items' cache files.
func (s *Store) DeleteItemDeep(id int64) ([]int64, error) {
	return s.deleteItemsDeep([]int64{id})
}

// DeleteVanishedDeep is DeleteVanished that also returns the removed
// item ids (for cache purging).
func (s *Store) DeleteVanishedDeep(libraryID int64, seen map[string]struct{}) (int, []int64, error) {
	rows, err := s.db.Query(`SELECT id, path FROM items WHERE library_id = ?`, libraryID)
	if err != nil {
		return 0, nil, err
	}
	var doomed []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return 0, nil, err
		}
		if _, ok := seen[p]; !ok {
			doomed = append(doomed, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	var removed []int64
	for _, chunk := range chunkIDs(doomed, 500) {
		ids, err := s.deleteItemsDeep(chunk)
		if err != nil {
			return len(removed), removed, err
		}
		removed = append(removed, ids...)
	}
	return len(removed), removed, nil
}

// DeleteLibraryDeep removes a library: every item in it is
// deep-deleted first, then the library row and its root paths.
// Returns the removed item ids. sql.ErrNoRows when the library does
// not exist. Disk files are never touched.
func (s *Store) DeleteLibraryDeep(id int64) ([]int64, error) {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM libraries WHERE id = ?`, id).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, sql.ErrNoRows
	}
	rows, err := s.db.Query(`SELECT id FROM items WHERE library_id = ?`, id)
	if err != nil {
		return nil, err
	}
	var itemIDs []int64
	for rows.Next() {
		var iid int64
		if err := rows.Scan(&iid); err != nil {
			rows.Close()
			return nil, err
		}
		itemIDs = append(itemIDs, iid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var removed []int64
	for _, chunk := range chunkIDs(itemIDs, 500) {
		ids, err := s.deleteItemsDeep(chunk)
		if err != nil {
			return removed, err
		}
		removed = append(removed, ids...)
	}
	if _, err := s.db.Exec(`DELETE FROM library_paths WHERE library_id = ?`, id); err != nil {
		return removed, err
	}
	if _, err := s.db.Exec(`DELETE FROM libraries WHERE id = ?`, id); err != nil {
		return removed, err
	}
	return removed, nil
}
