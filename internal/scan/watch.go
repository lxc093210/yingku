package scan

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"yingku/internal/store"
)

// Watcher keeps library contents fresh by watching every library root
// with inotify. Filesystem changes mark the owning library dirty; once
// a library has been quiet for the debounce window, an incremental
// scan of just that library runs (the scan itself prunes items whose
// files vanished, so deletions propagate without a manual rescan).
//
// The watcher is deliberately conservative: it never starts a scan
// while another scan runs, it coalesces bursts (an organize run can
// touch thousands of files), and a periodic root refresh picks up
// libraries whose paths were edited in settings.
type Watcher struct {
	st *store.Store
	sc *Scanner

	mu       sync.Mutex
	roots    map[string]int64 // cleaned root path -> library id
	watched  map[string]bool  // directories with a live inotify watch
	dirty    map[int64]time.Time
	rootSig  string
	fw       *fsnotify.Watcher
	debounce time.Duration
}

const watchDebounce = 20 * time.Second

// mediaExts are the file extensions whose appearance, change or
// removal can affect the catalog. Directory events always count.
var mediaExts = map[string]bool{
	".strm": true, ".nfo": true, ".json": true,
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".srt": true, ".ass": true, ".ssa": true, ".vtt": true,
	".sup": true, ".idx": true, ".sub": true,
}

// NewWatcher builds a Watcher bound to the store and scanner.
func NewWatcher(st *store.Store, sc *Scanner) *Watcher {
	return &Watcher{
		st:       st,
		sc:       sc,
		roots:    map[string]int64{},
		watched:  map[string]bool{},
		dirty:    map[int64]time.Time{},
		debounce: watchDebounce,
	}
}

// Start creates the inotify watcher, registers all library roots and
// serves until ctx is done. Failures are logged, never fatal: the
// server keeps working with manual scans if watching is unavailable.
func (w *Watcher) Start(ctx context.Context) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watcher: cannot create inotify watcher: %v (auto-refresh off)", err)
		return
	}
	w.fw = fw
	w.refreshRoots()
	go w.loop(ctx)
}

// refreshRoots reloads the library path set and, when it changed,
// walks every root adding watches for all directories. Walking is
// skipped when the signature is unchanged.
func (w *Watcher) refreshRoots() {
	libs, err := w.st.ListLibraries()
	if err != nil {
		log.Printf("watcher: list libraries: %v", err)
		return
	}
	roots := map[string]int64{}
	var parts []string
	for _, lib := range libs {
		if lib.SourceType == store.SourceTypeAlist {
			continue // remote roots cannot be watched (batch 29)
		}
		for _, p := range lib.Paths {
			cp := filepath.Clean(p)
			roots[cp] = lib.ID
			parts = append(parts, cp)
		}
	}
	sort.Strings(parts)
	sig := strings.Join(parts, "\n")
	w.mu.Lock()
	if sig == w.rootSig {
		w.mu.Unlock()
		return
	}
	w.rootSig = sig
	w.roots = roots
	w.mu.Unlock()

	added := 0
	for root := range roots {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable subtree (e.g. FUSE hiccup): skip quietly
			}
			if d.IsDir() {
				if w.addWatch(path) {
					added++
				}
			}
			return nil
		})
	}
	w.mu.Lock()
	total := len(w.watched)
	w.mu.Unlock()
	w.sc.bump(func(s *Status) { s.WatchDirs = total })
	log.Printf("watcher: %d directories watched (%d new), %d library roots", total, added, len(roots))
}

// addWatch registers one directory, reporting whether it was new.
func (w *Watcher) addWatch(dir string) bool {
	w.mu.Lock()
	if w.watched[dir] {
		w.mu.Unlock()
		return false
	}
	w.watched[dir] = true
	w.mu.Unlock()
	if err := w.fw.Add(dir); err != nil {
		w.mu.Lock()
		delete(w.watched, dir)
		w.mu.Unlock()
		return false
	}
	return true
}

// libraryFor maps a filesystem path to its owning library by longest
// matching root prefix.
func (w *Watcher) libraryFor(path string) (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	best := -1
	var id int64
	for root, libID := range w.roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			if len(root) > best {
				best = len(root)
				id = libID
			}
		}
	}
	return id, best >= 0
}

func (w *Watcher) markDirty(libID int64) {
	w.mu.Lock()
	w.dirty[libID] = time.Now()
	w.mu.Unlock()
}

// relevantEvent decides whether an fsnotify event can change the
// catalog: creates/removes/renames of anything, writes to media
// files. Pure chmod noise is ignored.
func relevantEvent(ev fsnotify.Event, isDir bool) bool {
	if ev.Op&(fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
		return true
	}
	if ev.Op&fsnotify.Write != 0 {
		if isDir {
			return true
		}
		return mediaExts[strings.ToLower(filepath.Ext(ev.Name))]
	}
	return false
}

func (w *Watcher) loop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	refreshTick := time.NewTicker(15 * time.Minute)
	defer refreshTick.Stop()
	for {
		select {
		case <-ctx.Done():
			w.fw.Close()
			return
		case ev, ok := <-w.fw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			log.Printf("watcher: %v", err)
		case <-tick.C:
			w.fireDue()
		case <-refreshTick.C:
			w.refreshRoots()
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	if ev.Op&fsnotify.Create != 0 {
		// A new directory needs watches for itself and anything
		// already inside it (moves land fully populated).
		filepath.WalkDir(ev.Name, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				w.addWatch(path)
			}
			return nil
		})
	}
	libID, ok := w.libraryFor(ev.Name)
	if !ok {
		return
	}
	// Directory-ness is only knowable for creates; for remove/rename
	// the path is gone, so treat unknown as relevant — a spurious
	// extra scan is harmless, a missed deletion is not.
	isDir := false
	if ev.Op&fsnotify.Create != 0 {
		if st, err := os.Stat(ev.Name); err == nil {
			isDir = st.IsDir()
		}
	}
	if relevantEvent(ev, isDir) {
		w.markDirty(libID)
	}
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.mu.Lock()
		delete(w.watched, ev.Name)
		w.mu.Unlock()
	}
}

// fireDue starts scans for libraries whose debounce window elapsed.
// One scan at a time: if the scanner is busy the library stays dirty
// and is retried on the next tick.
func (w *Watcher) fireDue() {
	if w.sc.Status().State == "running" {
		return
	}
	now := time.Now()
	w.mu.Lock()
	var due []int64
	for libID, last := range w.dirty {
		if now.Sub(last) >= w.debounce {
			due = append(due, libID)
			delete(w.dirty, libID)
		}
	}
	w.mu.Unlock()
	for _, libID := range due {
		if err := w.sc.StartLibrary(libID); err != nil {
			// Lost a race with a manual scan: requeue.
			w.mu.Lock()
			if _, ok := w.dirty[libID]; !ok {
				w.dirty[libID] = time.Now()
			}
			w.mu.Unlock()
			return
		}
		log.Printf("watcher: library %d changed on disk, incremental scan started", libID)
		return // one at a time; the rest fire after this scan finishes
	}
}
