package api

import (
	"crypto/sha1"
	"fmt"
	"log"
	"path"
	"strconv"
	"strings"
	"time"

	"yingku/internal/alist"
	"yingku/internal/meta"
	"yingku/internal/scan"
	"yingku/internal/store"
)

// Alist connection settings (batch 29), stored in app_settings at
// the same tier as the TMDB key. The password is write-only through
// the settings API: GET reports only whether one is set.
const (
	settingAlistBaseURL  = "alist_base_url"
	settingAlistUsername = "alist_username"
	settingAlistPassword = "alist_password"
)

// rawURLCacheTTL is how long a resolved Alist raw_url is reused
// before re-asking the API (the URLs are signed; 5 minutes is well
// inside their validity while sparing the API a call per play).
const rawURLCacheTTL = 5 * time.Minute

// alistScanBatchSize is how many prepped folders one write
// transaction covers. Prep (TMDB matching) for a batch runs with
// NO transaction open; 100 keeps a single batch's prep time
// bounded while the write phase stays a short burst.
const alistScanBatchSize = 100

type rawURLEntry struct {
	url string
	at  time.Time
}

// alistClient returns the shared Alist client, rebuilt whenever the
// connection settings change (the fingerprint covers base/user and
// the password's length+hash, never the password itself in logs).
func (s *Server) alistClient() (*alist.Client, error) {
	base, err := s.st.GetSetting(settingAlistBaseURL)
	if err != nil {
		return nil, err
	}
	user, err := s.st.GetSetting(settingAlistUsername)
	if err != nil {
		return nil, err
	}
	pass, err := s.st.GetSetting(settingAlistPassword)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s|%s|%x", base, user, sha1.Sum([]byte(pass)))
	s.alistMu.Lock()
	defer s.alistMu.Unlock()
	if s.alistCli != nil && s.alistCliKey == key {
		return s.alistCli, nil
	}
	cli := alist.New(base, user, pass)
	s.alistCli = cli
	s.alistCliKey = key
	return cli, nil
}

// itemIsAlist reports whether the item's library is an Alist library.
func (s *Server) itemIsAlist(it *store.Item) bool {
	lib, err := s.st.GetLibrary(it.LibraryID)
	return err == nil && lib != nil && lib.SourceType == store.SourceTypeAlist
}

// alistRawURL resolves (and briefly caches) the raw playback URL of
// one remote FILE path. Callers must pass a concrete file, never a
// folder: folder-level sources go through alistPlayableSource first.
func (s *Server) alistRawURL(remotePath string) (string, error) {
	s.rawMu.Lock()
	if e, ok := s.rawCache[remotePath]; ok && time.Since(e.at) < rawURLCacheTTL {
		s.rawMu.Unlock()
		return e.url, nil
	}
	s.rawMu.Unlock()
	cli, err := s.alistClient()
	if err != nil {
		return "", err
	}
	if !cli.Configured() {
		return "", fmt.Errorf("alist connection not configured")
	}
	raw, err := cli.GetRawURL(remotePath)
	if err != nil {
		return "", err
	}
	s.rawMu.Lock()
	s.rawCache[remotePath] = rawURLEntry{url: raw, at: time.Now()}
	s.rawMu.Unlock()
	return raw, nil
}

// videoExts are the file extensions treated as video sources when a
// folder is resolved at playback time (batch 29; movies only).
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".ts": true,
	".m2ts": true, ".iso": true, ".rmvb": true, ".mov": true,
}

func isVideoFile(name string) bool {
	return videoExts[strings.ToLower(path.Ext(name))]
}

// alistNameFingerprint identifies one title folder by NAME alone
// (batch 29c): the scan never looks inside folders, so the name is
// all a rescan may compare. A folder whose name is unchanged keeps
// its item untouched.
func alistNameFingerprint(folderName string) string {
	sum := sha1.Sum([]byte(folderName))
	return fmt.Sprintf("%x", sum)
}

// scanAlistLibrary is the remote scanner for Alist libraries,
// injected into scan.Scanner.RemoteScan by New.
//
// THE MODEL (batch 29c, set by the user after the first version's
// per-folder listing tripped the cloud drive's risk control): an
// Alist library is NOT a local STRM library. Scanning lists ONLY
// the root's paginated listing — one 1,632-folder root costs ~4
// List calls — and everything else (title parsing, TMDB matching,
// poster fill, quality guess) is derived from the folder NAME.
// The scan makes ZERO per-item API calls; a folder is only ever
// listed when a user actually plays its item (alistPlayableSource),
// at human pace. Video files are never opened, downloaded or
// probed; media info comes from filename inference alone.
//
// Shape handled: root → title folders (movies). Collection folders
// （系列/合集/N部 packs) are skipped. Folders that vanished from the
// root listing are pruned with the batch-28 deep delete. Items
// whose source is already resolved to a concrete file (old-model
// rows, or items a user has played) are never downgraded back to
// folder level by a rescan.
func (s *Server) scanAlistLibrary(lib store.Library) (scan.RemoteResult, error) {
	var res scan.RemoteResult
	if len(lib.Paths) == 0 {
		return res, fmt.Errorf("alist library %d has no remote root", lib.ID)
	}
	root := lib.Paths[0]
	cli, err := s.alistClient()
	if err != nil {
		return res, err
	}
	if !cli.Configured() {
		return res, fmt.Errorf("alist connection not configured (set it in 设置 → 接入与安全)")
	}
	cfg, cfgErr := s.tmdbCfg()
	if cfgErr != nil {
		log.Printf("alist scan lib %d: tmdb config: %v (matching disabled)", lib.ID, cfgErr)
	}
	index, err := s.st.LoadIndex(lib.ID)
	if err != nil {
		return res, err
	}
	resolvedPaths, err := s.st.ResolvedItemPaths(lib.ID)
	if err != nil {
		return res, err
	}
	// The ONLY Alist call a scan is allowed: the root listing
	// (paginated inside the client). If it fails the scan aborts —
	// pruning below must never run on a partial view of the catalog.
	entries, err := cli.List(root)
	if err != nil {
		return res, fmt.Errorf("alist list %s: %w", root, err)
	}
	now := time.Now().Unix()
	// Every directory in the root listing is "seen": an item is
	// pruned only when its folder is GONE from the root, never
	// because the folder was skipped for another reason.
	seen := make(map[string]struct{}, len(entries))

	// Writes are strictly two-phase per batch (batch-29b fix): the
	// prep phase does all compute (name parsing, TMDB matching) and
	// assembles rows in memory WITHOUT opening a store transaction
	// — the store has a single connection (MaxOpenConns(1)), so a
	// Tx held open across network calls starves the whole API. Only
	// when a batch is fully prepped does flushBatch open a Tx,
	// write the rows as fast as SQLite allows, and commit at once.
	// The index and resolved-paths set were loaded before the loop,
	// so prep never touches the store either.
	type matchedRef struct {
		id     int64
		tmdbID int
	}
	var matched []matchedRef

	type preparedWrite struct {
		it  *store.Item
		src store.MediaSource
	}
	var pending []preparedWrite
	flushBatch := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := s.st.Begin()
		if err != nil {
			return err
		}
		for _, p := range pending {
			id, err := tx.UpsertItem(p.it)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if err := tx.ReplaceSources(id, []store.MediaSource{p.src}); err != nil {
				_ = tx.Rollback()
				return err
			}
			if p.it.TmdbID > 0 {
				matched = append(matched, matchedRef{id: id, tmdbID: p.it.TmdbID})
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	for _, e := range entries {
		if !e.IsDir {
			continue // stray files at the root are not titles
		}
		folderPath := root + "/" + e.Name
		seen[folderPath] = struct{}{}
		if meta.IsAlistCollectionFolder(e.Name) {
			res.Skipped++
			continue
		}
		if resolvedPaths[folderPath] {
			// Already resolved to a concrete file (old-model row
			// or previously played): a rescan must never downgrade
			// it to an unresolved folder-level source.
			res.Skipped++
			continue
		}
		fp := alistNameFingerprint(e.Name)
		if ex, ok := index[folderPath]; ok && ex.Fingerprint == fp && ex.DateAdded > 0 {
			res.Skipped++
			continue
		}
		title, year := meta.ParseAlistFolderName(e.Name)
		tmdbID := 0
		if cfg.available() && title != "" {
			id, merr := matchTMDBMovie(cfg, title, year)
			if merr != nil {
				log.Printf("alist scan lib %d: tmdb match %q: %v", lib.ID, title, merr)
			} else {
				tmdbID = id
			}
		}
		// Quality guess from the FOLDER name: its tokens (4K
		// REMUX, 杜比视界…) are what the catalog itself advertises.
		mi := meta.InferFromFilename(e.Name)
		it := &store.Item{
			LibraryID:      lib.ID,
			Type:           "movie",
			Title:          title,
			Year:           year,
			TmdbID:         tmdbID,
			Path:           folderPath,
			SortTitle:      title,
			Fingerprint:    fp,
			CreatedAt:      now,
			UpdatedAt:      now,
			DateAdded:      now,
			SearchInitials: store.TitleInitials(title),
		}
		src := store.MediaSource{
			// Folder-level source: StrmPath is the folder itself
			// and Resolved is false. The concrete file is picked
			// at playback time (alistPlayableSource) — until
			// then the scan must not list inside the folder.
			StrmPath:        folderPath,
			TargetURL:       "",
			Width:           mi.Width,
			Height:          mi.Height,
			VideoCodec:      mi.VideoCodec,
			HDR:             mi.HDR,
			AudioSummary:    mi.AudioSummary,
			MediaInfoSource: mi.Source,
			Resolved:        false,
		}
		// Prep only: queue the row; the write happens in
		// flushBatch once the batch is fully prepped.
		pending = append(pending, preparedWrite{it: it, src: src})
		res.Items++
		if len(pending) >= alistScanBatchSize {
			if err := flushBatch(); err != nil {
				return res, fmt.Errorf("alist scan lib %d: commit batch: %w", lib.ID, err)
			}
		}
	}
	if err := flushBatch(); err != nil {
		return res, fmt.Errorf("alist scan lib %d: commit: %w", lib.ID, err)
	}

	// Prune folders that vanished from the root (deep delete).
	deleted, removedIDs, err := s.st.DeleteVanishedDeep(lib.ID, seen)
	if err != nil {
		return res, fmt.Errorf("alist scan lib %d: prune: %w", lib.ID, err)
	}
	if deleted > 0 {
		res.Deleted = deleted
		s.purgeItemCaches(removedIDs)
	}

	// Detail fill for matched items — the SAME fill the local
	// library uses (TMDB details + Chinese-title rule + lazy art
	// warm-up), run only after every write has committed.
	if cfg.available() {
		for _, m := range matched {
			s.fillAlistDetails(cfg, m.id, m.tmdbID)
			time.Sleep(150 * time.Millisecond)
		}
	}
	log.Printf("alist scan lib %d (%s): items=%d skipped=%d deleted=%d matched=%d",
		lib.ID, root, res.Items, res.Skipped, res.Deleted, len(matched))
	return res, nil
}

// alistPlayableSource returns the source playback should use for
// an Alist item. A resolved source (the normal case, and every
// old-model row) comes back untouched with ZERO Alist calls. An
// unresolved folder-level source is resolved NOW — this is the one
// moment the model allows API calls beyond the root listing,
// because a human just pressed play: list the item's own folder
// once, pick the largest video file, write the resolution back
// (one short UPDATE, after the network calls are done), and hand
// back the updated source. A folder with no video file at its top
// level (a collection pack that slipped past the name filter) is
// an error and stays unresolved.
func (s *Server) alistPlayableSource(it *store.Item, src store.MediaSource) (store.MediaSource, error) {
	if src.Resolved {
		return src, nil
	}
	// One resolution per item at a time: concurrent plays of the
	// same item share the first one's List + write-back.
	unlock := s.alistLocks.lock("resolve-" + strconv.FormatInt(it.ID, 10))
	defer unlock()
	// Another play may have resolved it while this one waited.
	if fresh, err := s.st.SourcesOf(it.ID); err == nil {
		for _, f := range fresh {
			if f.ID == src.ID && f.Resolved {
				return f, nil
			}
		}
	}
	cli, err := s.alistClient()
	if err != nil {
		return src, err
	}
	if !cli.Configured() {
		return src, fmt.Errorf("alist connection not configured")
	}
	files, err := cli.List(src.StrmPath) // StrmPath IS the folder here
	if err != nil {
		return src, fmt.Errorf("alist list %s: %w", src.StrmPath, err)
	}
	var best *alist.Entry
	for i := range files {
		f := files[i]
		if f.IsDir || !isVideoFile(f.Name) {
			continue
		}
		if best == nil || f.Size > best.Size {
			best = &f
		}
	}
	if best == nil {
		return src, fmt.Errorf("alist folder %s: no video file at top level", src.StrmPath)
	}
	mi := meta.InferFromFilename(best.Name)
	out := src
	out.StrmPath = src.StrmPath + "/" + best.Name
	out.Width, out.Height = mi.Width, mi.Height
	out.VideoCodec = mi.VideoCodec
	out.HDR = mi.HDR
	out.AudioSummary = mi.AudioSummary
	out.MediaInfoSource = mi.Source
	out.SizeBytes = best.Size
	out.Resolved = true
	if err := s.st.UpdateSourceResolved(out); err != nil {
		return src, fmt.Errorf("alist resolve item %d: write back: %w", it.ID, err)
	}
	log.Printf("alist resolve item %d: %s -> %s", it.ID, it.Path, out.StrmPath)
	return out, nil
}

// fillAlistDetails applies the standard TMDB detail fill to one
// freshly scanned matched item: overview/rating/genres/runtime,
// the Chinese-title preference, and poster/backdrop warm-up through
// the lazy-art cache (so the first client read already has art).
func (s *Server) fillAlistDetails(cfg tmdbConfig, itemID int64, tmdbID int) {
	title, overview, rating, genres, runtimeMin, err := fetchTMDBDetails(cfg, "movie", tmdbID)
	if err != nil {
		log.Printf("alist fill item %d: tmdb details: %v", itemID, err)
		return
	}
	if err := s.st.FillItemMeta(itemID, overview, rating, genres, runtimeMin); err != nil {
		log.Printf("alist fill item %d: %v", itemID, err)
		return
	}
	// Chinese-first title rule, same as the lazy fill in handleItem:
	// replace the current title only when TMDB's Chinese title has
	// Han characters and the current one has none.
	if title != "" && hasHan(title) {
		if it, err := s.st.GetItem(itemID); err == nil && it != nil && !hasHan(it.Title) {
			_ = s.st.FillItemTitle(itemID, title)
		}
	}
	_ = s.lazyTMDBArt(itemID, "poster")
	_ = s.lazyTMDBArt(itemID, "backdrop")
}
