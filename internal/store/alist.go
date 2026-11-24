package store

// Alist source resolution (batch 29c). The name-only remote scan
// writes folder-level sources (resolved = 0); the concrete file is
// picked at playback time and written back through
// UpdateSourceResolved. Local sources are always resolved.

// ResolvedItemPaths returns the set of item paths in one library
// that already own a resolved source — one whose strm_path names a
// concrete file rather than the item's folder itself. The remote
// scanner consults it so a rescan never downgrades an old-model
// (pre-29c) or already play-resolved source back to folder level.
func (s *Store) ResolvedItemPaths(libraryID int64) (map[string]bool, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT i.path FROM items i
		JOIN media_sources ms ON ms.item_id = i.id
		WHERE i.library_id = ? AND ms.resolved = 1 AND ms.strm_path <> i.path`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// UpdateSourceResolved writes a playback-time Alist resolution back
// into one source row: the concrete file path, the media info
// re-inferred from the real file name, its size, and resolved = 1.
// It is a single short UPDATE — callers must finish every network
// call before invoking it (the store has one connection).
func (s *Store) UpdateSourceResolved(ms MediaSource) error {
	_, err := s.db.Exec(`UPDATE media_sources SET
		strm_path = ?, width = ?, height = ?, video_codec = ?, hdr = ?,
		audio_summary = ?, mediainfo_source = ?, size_bytes = ?, resolved = 1
		WHERE id = ?`,
		ms.StrmPath, ms.Width, ms.Height, ms.VideoCodec, ms.HDR,
		ms.AudioSummary, ms.MediaInfoSource, ms.SizeBytes, ms.ID)
	return err
}
