package api

import (
	"crypto/sha1"
	"fmt"
	"log"
	"path"
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
// transaction covers. Prep (network + matching) for a batch runs
// with NO transaction open; 100 keeps a single batch's prep time
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
// one remote file path.
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

// videoExts are the file extensions a remote scan treats as video
// sources (batch 29; movies only).
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".ts": true,
	".m2ts": true, ".iso": true, ".rmvb": true, ".mov": true,
}

func isVideoFile(name string) bool {
	return videoExts[strings.ToLower(path.Ext(name))]
}

// alistFingerprint identifies one title folder's content snapshot:
// folder name + file count + total size. A rescan whose fingerprint
// matches the stored one skips the folder entirely.
func alistFingerprint(folderName string, fileCount int, totalSize int64) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d", folderName, fileCount, totalSize)))
	return fmt.Sprintf("%x", sum)
}

// scanAlistLibrary is the remote scanner for Alist libraries,
// injected into scan.Scanner.RemoteScan by New. It lists ONLY:
// the root, then each title folder's file list — video files are
// never opened, downloaded or probed (115 risk-control red line);
// media info comes from filename inference alone.
//
// Shape handled this batch: root → title folders → video files
// (movies). Title folders containing subdirectories are skipped and
// counted (series layout is out of scope); collection folders
// （系列/合集） are skipped. Folders that vanished from the root are
// pruned with the batch-28 deep delete.
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
	entries, err := cli.List(root)
	if err != nil {
		return res, fmt.Errorf("alist list %s: %w", root, err)
	}
	now := time.Now().Unix()
	seen := make(map[string]struct{}, len(entries))

	// Writes are strictly two-phase per batch (batch-29 fix): the
	// prep phase below does ALL network and compute work (Alist
	// listings, filename parsing, TMDB matching) and assembles the
	// rows in memory WITHOUT opening a store transaction — the
	// store has a single connection (MaxOpenConns(1)), so a Tx held
	// open across network calls starves the whole API. Only when a
	// batch is fully prepped does flushBatch open a Tx, write the
	// rows as fast as SQLite allows, and commit immediately. The
	// existing-item fingerprint index was loaded once, before the
	// loop (LoadIndex above), so prep never touches the store.
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
		if meta.IsAlistCollectionFolder(e.Name) {
			res.Skipped++
			continue
		}
		files, err := cli.List(folderPath)
		if err != nil {
			// Abort the whole scan: pruning below must never run
			// on a partial view of the catalog.
			return res, fmt.Errorf("alist list %s: %w", folderPath, err)
		}
		nested := false
		var videos []alist.Entry
		var totalSize int64
		for _, f := range files {
			if f.IsDir {
				nested = true
				break
			}
			totalSize += f.Size
			if isVideoFile(f.Name) {
				videos = append(videos, f)
			}
		}
		if nested {
			// Series-style layout (Season subfolders): out of
			// scope this batch — skip and count.
			log.Printf("alist scan lib %d: skip nested folder %s", lib.ID, folderPath)
			res.Skipped++
			continue
		}
		if len(videos) == 0 {
			res.Skipped++
			continue
		}
		best := videos[0]
		for _, v := range videos[1:] {
			if v.Size > best.Size {
				best = v
			}
		}
		fp := alistFingerprint(e.Name, len(files), totalSize)
		if ex, ok := index[folderPath]; ok && ex.Fingerprint == fp && ex.DateAdded > 0 {
			seen[folderPath] = struct{}{}
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
		mi := meta.InferFromFilename(best.Name)
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
			StrmPath:        folderPath + "/" + best.Name,
			TargetURL:       "",
			Width:           mi.Width,
			Height:          mi.Height,
			VideoCodec:      mi.VideoCodec,
			HDR:             mi.HDR,
			AudioSummary:    mi.AudioSummary,
			MediaInfoSource: mi.Source,
			SizeBytes:       best.Size,
		}
		// Prep only: queue the row; the write happens in
		// flushBatch once the batch is fully prepped.
		pending = append(pending, preparedWrite{it: it, src: src})
		seen[folderPath] = struct{}{}
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
