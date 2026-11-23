// Package scan walks a library root and upserts items into the store.
//
// Layout: <root>/{电影|电视剧}/<分类>/<片名 (年份) {tmdbid-N}>/[Season N/]<file>.strm
// Metadata priority (fixed by design): folder {tmdbid} > sibling .nfo >
// filename; media info: NanShare MediaInfo plugin sidecar (-mediainfo.json)
// via the MediaInfoProvider seam > filename inference > none.
package scan

import (
	"encoding/json"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"yingku/internal/meta"
	"yingku/internal/store"
)

// Status is the public scan state exposed via /api/scan/status.
type Status struct {
	State     string `json:"state"` // idle | running | done | error
	Root      string `json:"root"`
	LibraryID int64  `json:"libraryId,omitempty"` // set for single-library scans
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"finishedAt"`
	Libraries int    `json:"libraries"`
	Items     int    `json:"items"`    // items written (created or updated)
	Episodes  int    `json:"episodes"` // episode items written
	Skipped   int    `json:"skipped"`  // unchanged items skipped
	Deleted   int    `json:"deleted"`  // vanished items deleted
	Error     string `json:"error,omitempty"`
}

// CurrentScanLogicVersion is bumped whenever scanning logic changes in a
// way that must also reach items whose files did not change (batch 6:
// strict per-episode subtitle matching). The version is tracked per
// library: a library whose stored version lags is fully reprocessed on
// its next scan — all fingerprint skips disabled — and its version is
// written back when that scan completes.
const CurrentScanLogicVersion = 2

const scanLogicVersionKey = "scan_logic_version"

// Scanner runs library scans, at most one at a time.
type Scanner struct {
	st        *store.Store
	providers []meta.MediaInfoProvider

	// forceFull is armed for the duration of a run whose stored
	// scan-logic version lags CurrentScanLogicVersion.
	forceFull bool

	// groups accumulates movie folders sharing one TMDB id during a
	// library scan; they are written as ONE item with every folder's
	// sources (Emby-style version merging) when the scan flushes.
	groups map[int64]*movieGroup

	mu     sync.Mutex
	status Status
}

// New builds a Scanner with the local sidecar media-info provider.
func New(st *store.Store) *Scanner {
	return &Scanner{
		st:        st,
		providers: []meta.MediaInfoProvider{meta.SidecarProvider{}},
		status:    Status{State: "idle"},
	}
}

// Status returns a snapshot of the current scan state.
func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// StartAll launches an asynchronous scan of every library. seedRoot is
// only used to seed libraries when the library table is still empty.
func (s *Scanner) StartAll(seedRoot string) error {
	return s.launch(seedRoot, 0, func() error { return s.ScanAll(seedRoot) })
}

// StartLibrary launches an asynchronous scan of a single library.
func (s *Scanner) StartLibrary(id int64) error {
	return s.launch("", id, func() error { return s.ScanLibraryByID(id) })
}

// Start is kept for compatibility: it scans all libraries, seeding from
// root first when the library table is empty.
func (s *Scanner) Start(root string) error { return s.StartAll(root) }

func (s *Scanner) launch(rootLabel string, libID int64, fn func() error) error {
	s.mu.Lock()
	if s.status.State == "running" {
		s.mu.Unlock()
		return fmt.Errorf("scan already running")
	}
	s.status = Status{State: "running", Root: rootLabel, LibraryID: libID, StartedAt: time.Now().Unix()}
	s.mu.Unlock()
	go func() {
		err := fn()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status.EndedAt = time.Now().Unix()
		if err != nil {
			s.status.State = "error"
			s.status.Error = err.Error()
		} else {
			s.status.State = "done"
		}
	}()
	return nil
}

func (s *Scanner) bump(fn func(*Status)) {
	s.mu.Lock()
	fn(&s.status)
	s.mu.Unlock()
}

var kindByTop = map[string]string{"电影": "movie", "电视剧": "series"}

// Scan seeds libraries from root if needed, then scans all libraries.
// Kept as the entry point used by tests and first-run flows.
func (s *Scanner) Scan(root string) error { return s.ScanAll(root) }

// EnsureSeeded creates libraries from root's second-level folders
// (<root>/{电影|电视剧}/<分类>) when the library table is empty. It is a
// no-op when libraries already exist or root is empty.
func (s *Scanner) EnsureSeeded(root string) error {
	if root == "" {
		return nil
	}
	n, err := s.st.CountLibraries()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	root = filepath.Clean(root)
	top, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		kind, ok := kindByTop[e.Name()]
		if !ok {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			return err
		}
		for _, se := range sub {
			if !se.IsDir() || strings.HasPrefix(se.Name(), ".") {
				continue
			}
			p := filepath.Join(root, e.Name(), se.Name())
			if _, err := s.st.UpsertLibrary(se.Name(), kind, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// logicVersionKey is the app_settings key holding one library's
// scan-logic version.
func logicVersionKey(libID int64) string {
	return scanLogicVersionKey + ":" + strconv.FormatInt(libID, 10)
}

// storedLogicVersion returns the library's persisted scan-logic version
// (0 when the key is unset or unparsable, e.g. databases written before
// the key existed).
func (s *Scanner) storedLogicVersion(libID int64) int {
	v, err := s.st.GetSetting(logicVersionKey(libID))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

// beginRun arms a forced full reprocess for the scan of one library when
// its stored scan-logic version lags CurrentScanLogicVersion, and
// reports whether it did.
func (s *Scanner) beginRun(libID int64) bool {
	s.forceFull = s.storedLogicVersion(libID) < CurrentScanLogicVersion
	return s.forceFull
}

// unchanged reports whether the indexed item can be skipped: same
// fingerprint, date_added already backfilled, and this run is not a
// forced full reprocess. The date_added backfill and the logic-version
// force are independent one-shot triggers; either one reprocesses.
func (s *Scanner) unchanged(ex store.IndexEntry, fp string) bool {
	return !s.forceFull && ex.Fingerprint == fp && ex.DateAdded > 0
}

// ScanAll synchronously scans every library's root paths, seeding from
// seedRoot first when the library table is empty.
func (s *Scanner) ScanAll(seedRoot string) error {
	if err := s.EnsureSeeded(seedRoot); err != nil {
		return err
	}
	libs, err := s.st.ListLibraries()
	if err != nil {
		return err
	}
	owners := buildOwners(libs)
	for _, lib := range libs {
		if err := s.scanLibrary(lib, owners); err != nil {
			return err
		}
		s.bump(func(st *Status) { st.Libraries++ })
	}
	return nil
}

// ScanLibraryByID synchronously scans one library.
func (s *Scanner) ScanLibraryByID(id int64) error {
	lib, err := s.st.GetLibrary(id)
	if err != nil {
		return err
	}
	if lib == nil {
		return fmt.Errorf("library %d not found", id)
	}
	libs, err := s.st.ListLibraries()
	if err != nil {
		return err
	}
	if err := s.scanLibrary(*lib, buildOwners(libs)); err != nil {
		return err
	}
	s.bump(func(st *Status) { st.Libraries++ })
	return nil
}

// rootOwner maps one library root path to its library.
type rootOwner struct {
	libID int64
	path  string
}

func buildOwners(libs []store.Library) []rootOwner {
	var out []rootOwner
	for _, l := range libs {
		for _, p := range l.Paths {
			out = append(out, rootOwner{libID: l.ID, path: filepath.Clean(p)})
		}
	}
	return out
}

// ownerOf returns the library whose root is the longest prefix of folder
// (0 when no root matches).
func ownerOf(owners []rootOwner, folder string) int64 {
	best := 0
	var id int64
	for _, o := range owners {
		if folder == o.path || strings.HasPrefix(folder, o.path+string(filepath.Separator)) {
			if len(o.path) > best {
				best = len(o.path)
				id = o.libID
			}
		}
	}
	return id
}

// writer batches store writes, committing every maxOps item writes.
type writer struct {
	st  *store.Store
	tx  *store.Tx
	ops int
}

const maxOps = 500

func (w *writer) ensure() error {
	if w.tx == nil {
		tx, err := w.st.Begin()
		if err != nil {
			return err
		}
		w.tx = tx
		w.ops = 0
	}
	return nil
}

func (w *writer) flush() error {
	if w.tx == nil {
		return nil
	}
	err := w.tx.Commit()
	w.tx = nil
	w.ops = 0
	return err
}

func (w *writer) add() error {
	w.ops++
	if w.ops >= maxOps {
		return w.flush()
	}
	return nil
}

type folderCtx struct {
	itemDir    string
	files      []string            // files directly inside itemDir
	seasonDirs map[string][]string // season dir -> its files
	seasonNums map[string]int
}

// scanLibrary scans all root paths of one library, then deletes its
// vanished items. Item folders whose longest-prefix owner is a different
// library are skipped (the owning library's scan handles them). When the
// library's stored scan-logic version lags CurrentScanLogicVersion the
// scan reprocesses every item (see beginRun) and writes the new version
// back on success.
func (s *Scanner) scanLibrary(lib store.Library, owners []rootOwner) error {
	force := s.beginRun(lib.ID)
	defer func() { s.forceFull = false }()
	index, err := s.st.LoadIndex(lib.ID)
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	w := &writer{st: s.st}
	now := time.Now().Unix()
	s.groups = map[int64]*movieGroup{}
	defer func() { s.groups = nil }()
	for _, libPath := range lib.Paths {
		if err := s.scanPath(lib, libPath, owners, index, seen, w, now); err != nil {
			return err
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	if len(s.groups) > 0 {
		gw := &writer{st: s.st}
		if err := s.flushMovieGroups(gw, index, seen, lib.ID, now); err != nil {
			return err
		}
		if err := gw.flush(); err != nil {
			return err
		}
	}
	deleted, err := s.st.DeleteVanished(lib.ID, seen)
	if err != nil {
		return err
	}
	if deleted > 0 {
		s.bump(func(st *Status) { st.Deleted += deleted })
	}
	if force {
		if err := s.st.SetSetting(logicVersionKey(lib.ID), strconv.Itoa(CurrentScanLogicVersion)); err != nil {
			return err
		}
	}
	return nil
}

// rootKind classifies what a library root path itself is.
type rootKind int

const (
	rootContainer rootKind = iota // a folder of item folders (the usual case)
	rootMovie                     // the root is one movie's own folder
	rootSeries                    // the root is one series' own folder
)

// classifyRoot decides whether a library root path is itself a single
// item folder instead of a container of item folders. A tvshow.nfo, or a
// season dir holding .strm files, makes the root a series; .strm files
// directly inside make it a movie; anything else stays a container and
// keeps the per-folder walk unchanged.
func classifyRoot(libPath string) rootKind {
	// A root literally named like a season dir ("Season 1", "S01") is a
	// picker mistake, not an item folder: keep the old no-op behaviour.
	if _, ok := meta.ParseSeasonDir(filepath.Base(libPath)); ok {
		return rootContainer
	}
	entries, err := os.ReadDir(libPath)
	if err != nil {
		return rootContainer
	}
	hasStrm := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(e.Name(), "tvshow.nfo") {
			return rootSeries
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".strm") {
			hasStrm = true
		}
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, ok := meta.ParseSeasonDir(e.Name()); !ok {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(libPath, e.Name()))
		if err != nil {
			continue
		}
		for _, se := range sub {
			if !se.IsDir() && strings.EqualFold(filepath.Ext(se.Name()), ".strm") {
				return rootSeries
			}
		}
	}
	if hasStrm {
		return rootMovie
	}
	return rootContainer
}

// loadRootFolder builds the folder context for a root that is itself an
// item folder: its direct files plus the direct files of each season dir.
func loadRootFolder(libPath string) (*folderCtx, error) {
	ctx := &folderCtx{itemDir: libPath, seasonDirs: map[string][]string{}, seasonNums: map[string]int{}}
	entries, err := os.ReadDir(libPath)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		p := filepath.Join(libPath, e.Name())
		if !e.IsDir() {
			ctx.files = append(ctx.files, p)
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		num, ok := meta.ParseSeasonDir(e.Name())
		if !ok {
			continue
		}
		sub, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		files := []string{}
		for _, se := range sub {
			if !se.IsDir() {
				files = append(files, filepath.Join(p, se.Name()))
			}
		}
		ctx.seasonDirs[p] = files
		ctx.seasonNums[p] = num
	}
	return ctx, nil
}

func (s *Scanner) scanPath(lib store.Library, libPath string, owners []rootOwner, index map[string]store.IndexEntry, seen map[string]struct{}, w *writer, now int64) error {
	libID, kind := lib.ID, lib.Kind

	// A root may point at one item's own folder (a movie folder holding
	// its .strm, or a series folder holding tvshow.nfo / Season dirs)
	// rather than at a container. Process the root itself in that case;
	// its own nature decides the kind, not the library's.
	switch rk := classifyRoot(libPath); rk {
	case rootSeries, rootMovie:
		ctx, err := loadRootFolder(libPath)
		if err != nil {
			return err
		}
		folderKind := "movie"
		if rk == rootSeries {
			folderKind = "series"
		}
		if err := s.processFolder(w, index, seen, libID, folderKind, ctx, now); err != nil {
			return err
		}
		return w.flush()
	}

	var cur *folderCtx
	flush := func() error {
		if cur == nil {
			return nil
		}
		folderKind := kind
		if folderKind == "mixed" {
			// Decide per folder: season dirs or a tvshow.nfo mean series.
			folderKind = "movie"
			if len(cur.seasonDirs) > 0 || findNamed(cur.files, "tvshow.nfo") != "" {
				folderKind = "series"
			}
		}
		err := s.processFolder(w, index, seen, libID, folderKind, cur, now)
		cur = nil
		return err
	}

	walkErr := filepath.WalkDir(libPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(libPath, path)
		if rerr != nil || rel == "." {
			return rerr
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			switch depth {
			case 1:
				if err := flush(); err != nil {
					return err
				}
				if ownerOf(owners, path) != libID {
					// Owned by another library (nested root) or by none:
					// that library's own scan handles it.
					return filepath.SkipDir
				}
				cur = &folderCtx{itemDir: path, seasonDirs: map[string][]string{}, seasonNums: map[string]int{}}
			case 2:
				if cur != nil && kind != "movie" {
					if n, ok := meta.ParseSeasonDir(d.Name()); ok {
						cur.seasonDirs[path] = nil
						cur.seasonNums[path] = n
						return nil
					}
				}
				return filepath.SkipDir
			default:
				return filepath.SkipDir
			}
			return nil
		}
		// Files: depth 2 = directly inside the item folder (movie files,
		// tvshow.nfo, root-level episode strms); depth 3 = inside a season dir.
		if cur == nil {
			return nil
		}
		switch depth {
		case 2:
			if filepath.Dir(path) == cur.itemDir {
				cur.files = append(cur.files, path)
			}
		case 3:
			parent := filepath.Dir(path)
			if _, ok := cur.seasonDirs[parent]; ok {
				cur.seasonDirs[parent] = append(cur.seasonDirs[parent], path)
			}
		}
		return nil
	})
	if walkErr == nil {
		walkErr = flush()
	}
	if walkErr != nil {
		return walkErr
	}
	return w.flush()
}

// --- per-folder processing ---

type pendingItem struct {
	item    *store.Item
	sources []store.MediaSource
	images  map[string]store.ImageFile
	subs    []store.Subtitle
	people  []store.Person // nil = leave existing people untouched
	countEp bool
}

// actorsToPeople converts NFO cast entries to store rows (always non-nil,
// so a re-scan with a cast-less NFO clears stale people).
func actorsToPeople(nfo *meta.NFO) []store.Person {
	out := []store.Person{}
	if nfo == nil {
		return out
	}
	for i, a := range nfo.Actors {
		kind := a.Type
		if kind == "" {
			kind = "Actor"
		}
		out = append(out, store.Person{
			Name: a.Name, Role: a.Role, Kind: kind, Thumb: a.Thumb, Ord: i,
		})
	}
	return out
}

func (s *Scanner) processFolder(w *writer, index map[string]store.IndexEntry, seen map[string]struct{}, libID int64, kind string, ctx *folderCtx, now int64) error {
	if kind == "movie" {
		return s.processMovieFolder(w, index, seen, libID, ctx, now)
	}
	return s.processSeriesFolder(w, index, seen, libID, ctx, now)
}

// writeOne upserts one item plus its sources/images/subtitles via the batch
// writer and records it in the index + seen set.
func (s *Scanner) writeOne(w *writer, index map[string]store.IndexEntry, seen map[string]struct{}, p pendingItem) (int64, error) {
	if err := w.ensure(); err != nil {
		return 0, err
	}
	id, err := w.tx.UpsertItem(p.item)
	if err != nil {
		return 0, err
	}
	p.item.ID = id
	if p.sources != nil {
		if err := w.tx.ReplaceSources(id, p.sources); err != nil {
			return 0, err
		}
	}
	if p.images != nil {
		if err := w.tx.ReplaceImages(id, p.images); err != nil {
			return 0, err
		}
	}
	if p.subs != nil {
		if err := w.tx.ReplaceSubtitles(id, p.subs); err != nil {
			return 0, err
		}
	}
	if p.people != nil {
		if err := w.tx.ReplacePeople(id, p.people); err != nil {
			return 0, err
		}
	}
	index[p.item.Path] = store.IndexEntry{ID: id, Fingerprint: p.item.Fingerprint, DateAdded: p.item.DateAdded}
	seen[p.item.Path] = struct{}{}
	s.bump(func(st *Status) {
		st.Items++
		if p.countEp {
			st.Episodes++
		}
	})
	return id, w.add()
}

// skip records an unchanged item as seen.
func (s *Scanner) skip(seen map[string]struct{}, path string) {
	seen[path] = struct{}{}
	s.bump(func(st *Status) { st.Skipped++ })
}

// movieGroup collects every folder of one TMDB id seen during a
// library scan so they can be merged into a single multi-version
// item at flush time.
type movieGroup struct {
	folders []*movieFolderRef
}

type movieFolderRef struct {
	ctx      *folderCtx
	strms    []string
	sidecars []string
}

func (s *Scanner) processMovieFolder(w *writer, index map[string]store.IndexEntry, seen map[string]struct{}, libID int64, ctx *folderCtx, now int64) error {
	strms := filterExt(ctx.files, ".strm")
	if len(strms) == 0 {
		return nil
	}
	sidecars := sidecarsOf(strms)
	fi := meta.ParseFolderName(filepath.Base(ctx.itemDir))
	// Movies with a TMDB id are version-merged: folders sharing the
	// id (different releases, or the same library reached through
	// several roots) become one item. Registration only records the
	// folder; building and writing happen in flushMovieGroups.
	if fi.Tmdb > 0 && s.groups != nil {
		key := int64(fi.Tmdb)
		g := s.groups[key]
		if g == nil {
			g = &movieGroup{}
			s.groups[key] = g
		}
		g.folders = append(g.folders, &movieFolderRef{ctx: ctx, strms: strms, sidecars: sidecars})
		return nil
	}
	fp := fingerprint(strms, sidecars)
	// A stored date_added of 0 marks a legacy row: reprocess once even
	// when the fingerprint is unchanged so the backfill lands (see
	// unchanged for the combined skip rule).
	if ex, ok := index[ctx.itemDir]; ok && s.unchanged(ex, fp) {
		s.skip(seen, ctx.itemDir)
		return nil
	}
	_, err := s.writeOne(w, index, seen, s.buildMoviePending(libID, ctx, strms, sidecars, fp, now))
	return err
}

// buildMoviePending assembles one movie folder's item payload:
// metadata from the folder name + sibling NFO, one source per strm
// with its resolved media info, the folder's images, subtitles and
// cast. Used directly for unmerged movies and per folder for merged
// version groups.
func (s *Scanner) buildMoviePending(libID int64, ctx *folderCtx, strms, sidecars []string, fp string, now int64) pendingItem {
	fi := meta.ParseFolderName(filepath.Base(ctx.itemDir))
	it := &store.Item{
		LibraryID: libID, Type: "movie", Path: ctx.itemDir,
		Title: fi.Title, Year: fi.Year, TmdbID: fi.Tmdb,
		SortTitle: fi.Title, Fingerprint: fp, CreatedAt: now, UpdatedAt: now,
		DateAdded: maxMtime(append(append([]string{}, strms...), sidecars...)),
	}
	var firstMI meta.MediaInfo
	nfo := parseSiblingNFO(strms[0])
	if nfo != nil {
		applyNFO(it, nfo)
	}
	var sources []store.MediaSource
	for _, strm := range strms {
		mi := meta.ResolveMediaInfo(strm, s.providers)
		if firstMI.Source == "" {
			firstMI = mi
		}
		sources = append(sources, store.MediaSource{
			StrmPath: strm, TargetURL: readStrm(strm),
			Width: mi.Width, Height: mi.Height, VideoCodec: mi.VideoCodec,
			HDR: mi.HDR, AudioSummary: mi.AudioSummary, MediaInfoSource: mi.Source,
			StreamsJSON: streamsJSON(mi), SizeBytes: mi.Size, Bitrate: mi.Bitrate,
		})
	}
	if it.RuntimeMin == 0 {
		it.RuntimeMin = firstMI.RuntimeMin
	}
	return pendingItem{
		item: it, sources: sources,
		images: folderImages(ctx.files),
		subs:   matchSubtitles(ctx.files, len(strms), strms),
		people: actorsToPeople(nfo),
	}
}

// flushMovieGroups writes every registered TMDB group as one merged
// item: the best folder (highest-resolution source, then path)
// supplies the metadata and primary path, all folders contribute
// their sources in quality order, and pre-existing duplicate items
// are folded in — user data migrates to the survivor, the rest are
// deleted. A group whose combined fingerprint still matches the
// stored item is skipped whole.
func (s *Scanner) flushMovieGroups(w *writer, index map[string]store.IndexEntry, seen map[string]struct{}, libID int64, now int64) error {
	keys := make([]int64, 0, len(s.groups))
	for k := range s.groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, tmdb := range keys {
		g := s.groups[tmdb]
		var allStrms, allSide []string
		for _, f := range g.folders {
			allStrms = append(allStrms, f.strms...)
			allSide = append(allSide, f.sidecars...)
		}
		combined := fingerprint(allStrms, allSide)
		existing, err := s.st.ItemsByTmdb(libID, tmdb)
		if err != nil {
			return err
		}
		if len(existing) == 1 && !s.forceFull && existing[0].Fingerprint == combined && existing[0].DateAdded > 0 {
			seen[existing[0].Path] = struct{}{}
			for range g.folders {
				s.bump(func(st *Status) { st.Skipped++ })
			}
			continue
		}
		type built struct {
			p         pendingItem
			bestW     int
			bestH     int
		}
		var bs []built
		for _, f := range g.folders {
			p := s.buildMoviePending(libID, f.ctx, f.strms, f.sidecars, "", now)
			bw, bh := 0, 0
			for _, src := range p.sources {
				if src.Width > bw || (src.Width == bw && src.Height > bh) {
					bw, bh = src.Width, src.Height
				}
			}
			bs = append(bs, built{p: p, bestW: bw, bestH: bh})
		}
		sort.SliceStable(bs, func(i, j int) bool {
			if bs[i].bestW != bs[j].bestW {
				return bs[i].bestW > bs[j].bestW
			}
			if bs[i].bestH != bs[j].bestH {
				return bs[i].bestH > bs[j].bestH
			}
			return bs[i].p.item.Path < bs[j].p.item.Path
		})
		var sources []store.MediaSource
		for _, b := range bs {
			sources = append(sources, b.p.sources...)
		}
		sort.SliceStable(sources, func(i, j int) bool {
			if sources[i].Width != sources[j].Width {
				return sources[i].Width > sources[j].Width
			}
			if sources[i].Height != sources[j].Height {
				return sources[i].Height > sources[j].Height
			}
			return sources[i].StrmPath < sources[j].StrmPath
		})
		images := map[string]store.ImageFile{}
		for _, b := range bs {
			for k, v := range b.p.images {
				if _, ok := images[k]; !ok {
					images[k] = v
				}
			}
		}
		subSeen := map[string]bool{}
		var subs []store.Subtitle
		for _, b := range bs {
			for _, sub := range b.p.subs {
				if !subSeen[sub.Path] {
					subSeen[sub.Path] = true
					subs = append(subs, sub)
				}
			}
		}
		var people []store.Person
		for _, b := range bs {
			if len(b.p.people) > 0 {
				people = b.p.people
				break
			}
		}
		item := bs[0].p.item
		item.Fingerprint = combined
		var maxAdded int64
		for _, b := range bs {
			if b.p.item.DateAdded > maxAdded {
				maxAdded = b.p.item.DateAdded
			}
		}
		item.DateAdded = maxAdded
		if item.RuntimeMin == 0 {
			for _, b := range bs {
				if b.p.item.RuntimeMin > 0 {
					item.RuntimeMin = b.p.item.RuntimeMin
					break
				}
			}
		}
		if len(existing) > 0 {
			survivor := existing[0]
			for _, e := range existing {
				if e.Path == item.Path {
					survivor = e
					break
				}
			}
			var doomed []int64
			for _, e := range existing {
				if e.ID != survivor.ID {
					doomed = append(doomed, e.ID)
				}
			}
			if len(doomed) > 0 {
				if err := s.st.MergeUserDataInto(survivor.ID, doomed); err != nil {
					return err
				}
				for _, id := range doomed {
					if err := s.st.DeleteItemByID(id); err != nil {
						return err
					}
				}
				s.bump(func(st *Status) { st.Deleted += len(doomed) })
			}
			if survivor.Path != item.Path {
				if err := s.st.SetItemPath(survivor.ID, item.Path); err != nil {
					return err
				}
			}
		}
		if _, err := s.writeOne(w, index, seen, pendingItem{
			item: item, sources: sources, images: images, subs: subs, people: people,
		}); err != nil {
			return err
		}
		// Commit after every group: the store runs on a single
		// connection, and the next group's reads and user-data
		// merge run outside this writer — they would wait forever
		// on the connection this transaction is holding.
		if err := w.flush(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) processSeriesFolder(w *writer, index map[string]store.IndexEntry, seen map[string]struct{}, libID int64, ctx *folderCtx, now int64) error {
	// Any strm anywhere (root or season dirs) makes this a series.
	hasStrm := len(filterExt(ctx.files, ".strm")) > 0
	for _, files := range ctx.seasonDirs {
		if len(filterExt(files, ".strm")) > 0 {
			hasStrm = true
		}
	}
	if !hasStrm {
		return nil
	}
	fi := meta.ParseFolderName(filepath.Base(ctx.itemDir))
	series := &store.Item{
		LibraryID: libID, Type: "series", Path: ctx.itemDir,
		Title: fi.Title, Year: fi.Year, TmdbID: fi.Tmdb,
		SortTitle: fi.Title, CreatedAt: now, UpdatedAt: now,
		DateAdded: maxMtime(ctx.files),
	}
	tvNFO := findNamed(ctx.files, "tvshow.nfo")
	fpFiles := []string{}
	if tvNFO != "" {
		fpFiles = append(fpFiles, tvNFO)
	}
	series.Fingerprint = fingerprint(fpFiles, nil)
	var seriesID int64
	if ex, ok := index[ctx.itemDir]; ok && s.unchanged(ex, series.Fingerprint) {
		s.skip(seen, ctx.itemDir)
		seriesID = ex.ID
	} else {
		var seriesPeople []store.Person
		if tvNFO != "" {
			if nfo, err := meta.ParseNFOFile(tvNFO); err == nil {
				applyNFO(series, nfo)
				seriesPeople = actorsToPeople(nfo)
			}
		}
		if seriesPeople == nil {
			seriesPeople = []store.Person{}
		}
		id, err := s.writeOne(w, index, seen, pendingItem{
			item: series, images: folderImages(ctx.files), subs: []store.Subtitle{},
			people: seriesPeople,
		})
		if err != nil {
			return err
		}
		seriesID = id
	}

	// Seasons: physical dirs first, then implicit seasons from root strms.
	seasonIDs := map[int]int64{}
	seasonPaths := map[int]string{}
	ensureSeason := func(num int, dirPath string, files []string) (int64, error) {
		if id, ok := seasonIDs[num]; ok {
			return id, nil
		}
		path := dirPath
		if path == "" {
			path = filepath.Join(ctx.itemDir, fmt.Sprintf("Season %d", num))
		}
		st := &store.Item{
			LibraryID: libID, Type: "season", Path: path,
			ParentID: &seriesID, SeriesID: &seriesID, SeasonNum: &num,
			Title: fmt.Sprintf("季 %d", num), SortTitle: fmt.Sprintf("季 %d", num),
			CreatedAt: now, UpdatedAt: now, DateAdded: maxMtime(files),
		}
		fpF := []string{}
		if dirPath != "" {
			if sn := findNamed(files, "season.nfo"); sn != "" {
				fpF = append(fpF, sn)
				if nfo, err := meta.ParseNFOFile(sn); err == nil {
					if nfo.Title != "" {
						st.Title = nfo.Title
						st.SortTitle = nfo.Title
					}
					st.Year = nfo.Year
					st.Overview = nfo.Plot
				}
			}
		}
		st.Fingerprint = fingerprint(fpF, nil)
		if ex, ok := index[path]; ok && s.unchanged(ex, st.Fingerprint) {
			s.skip(seen, path)
			seasonIDs[num] = ex.ID
			seasonPaths[num] = path
			return ex.ID, nil
		}
		id, err := s.writeOne(w, index, seen, pendingItem{item: st, subs: []store.Subtitle{}})
		if err != nil {
			return 0, err
		}
		seasonIDs[num] = id
		seasonPaths[num] = path
		return id, nil
	}

	// Episodes grouped by season dir.
	type epJob struct {
		strm     string
		seasonID int64
		seasonN  int
		files    []string
	}
	var jobs []epJob
	seasonDirsSorted := make([]string, 0, len(ctx.seasonDirs))
	for d := range ctx.seasonDirs {
		seasonDirsSorted = append(seasonDirsSorted, d)
	}
	sort.Strings(seasonDirsSorted)
	for _, dir := range seasonDirsSorted {
		num := ctx.seasonNums[dir]
		sid, err := ensureSeason(num, dir, ctx.seasonDirs[dir])
		if err != nil {
			return err
		}
		for _, strm := range filterExt(ctx.seasonDirs[dir], ".strm") {
			jobs = append(jobs, epJob{strm: strm, seasonID: sid, seasonN: num, files: ctx.seasonDirs[dir]})
		}
	}
	for _, strm := range filterExt(ctx.files, ".strm") {
		sn, _, ok := meta.ParseSxExx(filepath.Base(strm))
		if !ok {
			sn = 1
		}
		sid, err := ensureSeason(sn, "", nil)
		if err != nil {
			return err
		}
		jobs = append(jobs, epJob{strm: strm, seasonID: sid, seasonN: sn, files: ctx.files})
	}

	for _, job := range jobs {
		if err := s.processEpisode(w, index, seen, libID, seriesID, job.seasonID, job.seasonN, job.strm, job.files, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) processEpisode(w *writer, index map[string]store.IndexEntry, seen map[string]struct{}, libID, seriesID, seasonID int64, seasonNum int, strm string, siblings []string, now int64) error {
	fp := fingerprint([]string{strm}, sidecarsOf([]string{strm}))
	if ex, ok := index[strm]; ok && s.unchanged(ex, fp) {
		s.skip(seen, strm)
		return nil
	}
	_, epFromName, _ := meta.ParseSxExx(filepath.Base(strm))
	ep := &store.Item{
		LibraryID: libID, Type: "episode", Path: strm,
		ParentID: &seasonID, SeriesID: &seriesID,
		Title: episodeTitleFallback(strm), Fingerprint: fp,
		CreatedAt: now, UpdatedAt: now,
		DateAdded: maxMtime(sameStemFiles(siblings, strm)),
	}
	sn, en := seasonNum, epFromName
	epNFO := parseSiblingNFO(strm)
	if nfo := epNFO; nfo != nil {
		if nfo.Title != "" {
			ep.Title = nfo.Title
		}
		ep.Overview = nfo.Plot
		ep.Rating = nfo.RatingValue()
		ep.RuntimeMin = nfo.Runtime
		ep.Year = nfo.Year
		if nfo.SeasonNumber() > 0 {
			sn = nfo.SeasonNumber()
		}
		if nfo.Episode > 0 {
			en = nfo.Episode
		}
		if nfo.SortTitle != "" {
			ep.SortTitle = nfo.SortTitle
		}
	}
	if ep.SortTitle == "" {
		ep.SortTitle = ep.Title
	}
	ep.SeasonNum = &sn
	ep.EpisodeNum = &en
	mi := meta.ResolveMediaInfo(strm, s.providers)
	if ep.RuntimeMin == 0 {
		ep.RuntimeMin = mi.RuntimeMin
	}
	_, err := s.writeOne(w, index, seen, pendingItem{
		item: ep, countEp: true,
		sources: []store.MediaSource{{
			StrmPath: strm, TargetURL: readStrm(strm),
			Width: mi.Width, Height: mi.Height, VideoCodec: mi.VideoCodec,
			HDR: mi.HDR, AudioSummary: mi.AudioSummary, MediaInfoSource: mi.Source,
			StreamsJSON: streamsJSON(mi), SizeBytes: mi.Size, Bitrate: mi.Bitrate,
		}},
		subs:   matchSubtitles(siblings, len(filterExt(siblings, ".strm")), []string{strm}),
		people: actorsToPeople(epNFO),
	})
	return err
}

// --- helpers ---

func applyNFO(it *store.Item, nfo *meta.NFO) {
	if nfo.Title != "" {
		it.Title = nfo.Title
	}
	if nfo.OriginalTitle != "" {
		it.OriginalTitle = nfo.OriginalTitle
	}
	if nfo.Plot != "" {
		it.Overview = nfo.Plot
	}
	if r := nfo.RatingValue(); r > 0 {
		it.Rating = r
	}
	if nfo.Year > 0 {
		it.Year = nfo.Year
	}
	if nfo.Runtime > 0 {
		it.RuntimeMin = nfo.Runtime
	}
	if len(nfo.Genres) > 0 {
		it.Genres = nfo.Genres
	}
	if it.TmdbID == 0 && nfo.TmdbID > 0 {
		it.TmdbID = nfo.TmdbID
	}
	if nfo.SortTitle != "" {
		it.SortTitle = nfo.SortTitle
	}
	if it.SortTitle == "" {
		it.SortTitle = it.Title
	}
}

func parseSiblingNFO(strm string) *meta.NFO {
	stem := strings.TrimSuffix(strm, filepath.Ext(strm))
	nfo, err := meta.ParseNFOFile(stem + ".nfo")
	if err != nil {
		return nil
	}
	return nfo
}

// streamsJSON serializes the full stream set of a resolved media info
// for storage; empty when the source carried no stream payload.
func streamsJSON(mi meta.MediaInfo) string {
	if len(mi.Streams) == 0 {
		return ""
	}
	raw, err := json.Marshal(mi.Streams)
	if err != nil {
		return ""
	}
	return string(raw)
}

// sidecarsOf returns the existing nfo/mediainfo sidecar paths for strms.
func sidecarsOf(strms []string) []string {
	var out []string
	for _, strm := range strms {
		stem := strings.TrimSuffix(strm, filepath.Ext(strm))
		for _, p := range []string{stem + ".nfo", meta.SidecarPath(strm)} {
			if _, err := os.Stat(p); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// fingerprint hashes (path, size, mtime) of the given files.
func fingerprint(primary, extra []string) string {
	files := append(append([]string{}, primary...), extra...)
	sort.Strings(files)
	h := sha1.New()
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			fmt.Fprintf(h, "%s|missing\n", f)
			continue
		}
		fmt.Fprintf(h, "%s|%d|%d\n", filepath.Base(f), st.Size(), st.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))
}

// maxMtime returns the newest modification time (unix seconds) among
// files; missing files are ignored and 0 means "none found".
func maxMtime(files []string) int64 {
	var best int64
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			if mt := st.ModTime().Unix(); mt > best {
				best = mt
			}
		}
	}
	return best
}

// sameStemFiles returns the subset of files sharing strm's stem: the
// .strm itself plus same-stem sidecars (x.nfo, x-mediainfo.json, ...).
func sameStemFiles(files []string, strm string) []string {
	stem := strings.TrimSuffix(strm, filepath.Ext(strm))
	var out []string
	for _, f := range files {
		if f == strm || strings.HasPrefix(f, stem+".") || f == stem+"-mediainfo.json" {
			out = append(out, f)
		}
	}
	return out
}

// readStrm returns the first line of a .strm file (the target URL, verbatim).
func readStrm(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(raw))
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return line
}

func filterExt(files []string, ext string) []string {
	var out []string
	for _, f := range files {
		if strings.EqualFold(filepath.Ext(f), ext) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func findNamed(files []string, name string) string {
	for _, f := range files {
		if strings.EqualFold(filepath.Base(f), name) {
			return f
		}
	}
	return ""
}

var posterCandidates = []string{"poster.jpg", "poster.png", "folder.jpg", "folder.png", "cover.jpg", "cover.png"}
var backdropCandidates = []string{"backdrop.jpg", "backdrop.png", "background.jpg", "background.png", "fanart.jpg", "fanart.png"}

// folderImages maps poster/backdrop files in an item folder. When several
// candidates exist for a kind, the one with the largest pixel dimensions
// wins (scrapers often leave a small backdrop.jpg next to a full-size
// background.jpg); ties keep the earlier name in the candidate list. The
// winning file's dimensions are recorded alongside its path so the hero
// rotation can tier candidates by backdrop resolution.
func folderImages(files []string) map[string]store.ImageFile {
	byName := map[string]string{}
	for _, f := range files {
		byName[strings.ToLower(filepath.Base(f))] = f
	}
	out := map[string]store.ImageFile{}
	for kind, candidates := range map[string][]string{"poster": posterCandidates, "backdrop": backdropCandidates} {
		var best store.ImageFile
		var bestScore int64 = -1
		for _, name := range candidates {
			p, ok := byName[name]
			if !ok {
				continue
			}
			w, h, dimsOK := imageDims(p)
			var score int64
			if dimsOK {
				score = int64(w) * int64(h)
			} else if st, err := os.Stat(p); err == nil {
				score = st.Size()
			}
			if score > bestScore {
				bestScore = score
				best = store.ImageFile{Path: p, Width: w, Height: h}
			}
		}
		if best.Path != "" {
			out[kind] = best
		}
	}
	return out
}

// imageScore scores an image file by pixel count, falling back to its
// byte size when the dimensions cannot be read.
func imageScore(path string) int64 {
	if w, h, ok := imageDims(path); ok {
		return int64(w) * int64(h)
	}
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

// imageDims reads the pixel dimensions of a JPEG or PNG from its header
// only (at most the first 512KB is read; dimensions live at the start).
func imageDims(path string) (int, int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	buf := make([]byte, 512<<10)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	// PNG: signature, then IHDR with big-endian width/height at 16/20.
	if n >= 24 && bytes.Equal(buf[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		w := int(binary.BigEndian.Uint32(buf[16:20]))
		h := int(binary.BigEndian.Uint32(buf[20:24]))
		if w > 0 && h > 0 {
			return w, h, true
		}
		return 0, 0, false
	}
	// JPEG: walk marker segments until a start-of-frame marker.
	if n >= 4 && buf[0] == 0xFF && buf[1] == 0xD8 {
		for i := 2; i+9 <= n; {
			if buf[i] != 0xFF {
				i++
				continue
			}
			marker := buf[i+1]
			if marker == 0xFF { // padding
				i++
				continue
			}
			// Standalone markers carry no length field.
			if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD9) {
				i += 2
				continue
			}
			segLen := int(binary.BigEndian.Uint16(buf[i+2 : i+4]))
			if segLen < 2 {
				return 0, 0, false
			}
			if marker == 0xC0 || marker == 0xC1 || marker == 0xC2 {
				h := int(binary.BigEndian.Uint16(buf[i+5 : i+7]))
				w := int(binary.BigEndian.Uint16(buf[i+7 : i+9]))
				if w > 0 && h > 0 {
					return w, h, true
				}
				return 0, 0, false
			}
			i += 2 + segLen
		}
	}
	return 0, 0, false
}

var subExts = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true}

var langTokens = []string{"zh-cn", "zh-tw", "zh-hans", "zh-hant", "chs", "cht", "chi", "zho", "eng", "jpn", "kor"}

// matchSubtitles assigns a folder's subtitle sidecars to the item(s)
// backed by targets. folderStrmCount is the number of .strm files the
// folder really holds: only when the target is the folder's sole strm
// does every subtitle in the folder belong to it. Otherwise — episodes
// sharing a season folder, multi-version movie folders — a subtitle
// must strictly match a target: its file name starts with the target's
// stem, or it carries the target's SxxExx season+episode token. (The
// episode call site used to pass just its own strm, which made every
// episode inherit the whole season's subtitles.)
func matchSubtitles(folderFiles []string, folderStrmCount int, targets []string) []store.Subtitle {
	var subs []string
	for _, f := range folderFiles {
		if subExts[strings.ToLower(filepath.Ext(f))] {
			subs = append(subs, f)
		}
	}
	if len(subs) == 0 || len(targets) == 0 {
		return nil
	}
	// The folder's only strm: every subtitle belongs to that item.
	if folderStrmCount == 1 && len(targets) == 1 {
		out := make([]store.Subtitle, 0, len(subs))
		for _, p := range subs {
			out = append(out, store.Subtitle{Path: p, Lang: subtitleLang(p)})
		}
		return out
	}
	type targetStem struct {
		stem          string
		season, epNum int
		hasSxExx      bool
	}
	stems := make([]targetStem, 0, len(targets))
	for _, t := range targets {
		stem := strings.TrimSuffix(filepath.Base(t), filepath.Ext(t))
		sn, ep, ok := meta.ParseSxExx(stem)
		stems = append(stems, targetStem{stem: stem, season: sn, epNum: ep, hasSxExx: ok})
	}
	var out []store.Subtitle
	for _, p := range subs {
		base := filepath.Base(p)
		matched := false
		for _, t := range stems {
			if strings.HasPrefix(base, t.stem) {
				matched = true
				break
			}
			if t.hasSxExx {
				if sn, ep, ok := meta.ParseSxExx(base); ok && sn == t.season && ep == t.epNum {
					matched = true
					break
				}
			}
		}
		if matched {
			out = append(out, store.Subtitle{Path: p, Lang: subtitleLang(p)})
		}
	}
	return out
}

func subtitleLang(path string) string {
	name := strings.ToLower(filepath.Base(path))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '.' || r == ' ' || r == '_' || r == '-'
	})
	joined := strings.Join(parts, "-")
	for _, tok := range langTokens {
		for _, p := range parts {
			if p == tok {
				return tok
			}
		}
		if strings.Contains(joined, tok) && (tok == "zh-cn" || tok == "zh-tw") {
			return tok
		}
	}
	return ""
}

func episodeTitleFallback(strm string) string {
	base := strings.TrimSuffix(filepath.Base(strm), filepath.Ext(strm))
	return base
}
