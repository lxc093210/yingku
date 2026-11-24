package api

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"yingku/internal/store"
)

// Media deletion (right-click menu in the web UI). Deleting is guarded:
// every target path must live inside a registered library root, and a
// library root itself is never a target. Files go first, then the item
// row (FK cascades clear children and dependent rows).

type deletePreviewDTO struct {
	Name       string   `json:"Name"`
	Type       string   `json:"Type"`
	Paths      []string `json:"Paths"`
	FileCount  int      `json:"FileCount"`
	TotalBytes int64    `json:"TotalBytes"`
}

// deleteTargets returns the on-disk paths that deleting it would remove.
//
// Item path semantics (fixed by the scanner): a movie's or series' path
// is its own folder; an episode's path is its .strm file, and deleting an
// episode removes the same-stem sidecar set (strm, nfo, -mediainfo.json,
// registered subtitles and images).
func (s *Server) deleteTargets(it *store.Item) ([]string, error) {
	switch it.Type {
	case "series":
		return []string{filepath.Clean(it.Path)}, nil
	case "movie":
		// A merged (multi-version) movie lives in several folders:
		// the primary one plus the folder of every source. Deleting
		// the item removes them all, like Emby deleting every
		// version's files; anything less would resurrect the movie
		// from the surviving folder on the next scan.
		seen := map[string]bool{}
		var out []string
		add := func(p string) {
			c := filepath.Clean(p)
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
		add(it.Path)
		sources, err := s.st.SourcesOf(it.ID)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			add(filepath.Dir(src.StrmPath))
		}
		return out, nil
	case "episode":
		strm := filepath.Clean(it.Path)
		stem := strings.TrimSuffix(strm, filepath.Ext(strm))
		seen := map[string]bool{}
		var out []string
		add := func(p string) {
			if p == "" {
				return
			}
			c := filepath.Clean(p)
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
		add(strm)
		add(stem + ".nfo")
		add(stem + "-mediainfo.json")
		subs, err := s.st.SubtitlesOf(it.ID)
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			add(sub.Path)
		}
		for _, kind := range []string{"poster", "backdrop"} {
			if p, err := s.st.ImagePath(it.ID, kind); err == nil {
				add(p)
			}
		}
		return out, nil
	}
	return nil, errBad("item type cannot be deleted")
}

// checkTargetsDeletable verifies every target lies within a library root
// and none of them is a library root itself.
func (s *Server) checkTargetsDeletable(targets []string) error {
	roots, err := s.st.LibraryPaths()
	if err != nil {
		return err
	}
	cleanRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		cleanRoots = append(cleanRoots, filepath.Clean(root))
	}
	for _, t := range targets {
		inside := false
		for _, root := range cleanRoots {
			if t == root {
				return errForbidden("refusing to delete a library root")
			}
			if withinRoot(root, t) {
				inside = true
				break
			}
		}
		if !inside {
			return errForbidden("path outside library: " + t)
		}
	}
	return nil
}

type forbidden string

func errForbidden(msg string) error { return forbidden(msg) }
func (e forbidden) Error() string   { return string(e) }

// previewFor builds the delete-preview payload for an item (shared by the
// preview endpoint; targets are validated by the DELETE handler itself).
func (s *Server) previewFor(it *store.Item) (*deletePreviewDTO, error) {
	targets, err := s.deleteTargets(it)
	if err != nil {
		return nil, err
	}
	dto := &deletePreviewDTO{
		Name: it.Title, Type: embyType[it.Type], Paths: targets,
	}
	for _, t := range targets {
		fi, err := os.Stat(t)
		if err != nil {
			continue // already gone from disk; nothing to count
		}
		if !fi.IsDir() {
			dto.FileCount++
			dto.TotalBytes += fi.Size()
			continue
		}
		_ = filepath.WalkDir(t, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				dto.FileCount++
				if info, err := d.Info(); err == nil {
					dto.TotalBytes += info.Size()
				}
			}
			return nil
		})
	}
	return dto, nil
}

func (s *Server) loadDeletableItem(w http.ResponseWriter, r *http.Request) *store.Item {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad item id")
		return nil
	}
	it, err := s.st.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	if it == nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return nil
	}
	if it.Type == "season" {
		writeErr(w, http.StatusBadRequest, "delete the whole series instead of one season")
		return nil
	}
	return it
}

func (s *Server) handleDeletePreview(w http.ResponseWriter, r *http.Request) {
	it := s.loadDeletableItem(w, r)
	if it == nil {
		return
	}
	dto, err := s.previewFor(it)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handleItemDelete(w http.ResponseWriter, r *http.Request) {
	it := s.loadDeletableItem(w, r)
	if it == nil {
		return
	}
	targets, err := s.deleteTargets(it)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Safety gate: validate ALL targets before touching anything.
	if err := s.checkTargetsDeletable(targets); err != nil {
		var fe forbidden
		if errors.As(err, &fe) {
			writeErr(w, http.StatusForbidden, fe.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, t := range targets {
		if err := os.RemoveAll(t); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete "+t+": "+err.Error())
			return
		}
	}
	removed, err := s.st.DeleteItemDeep(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(removed) == 0 {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	s.purgeItemCaches(removed)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
