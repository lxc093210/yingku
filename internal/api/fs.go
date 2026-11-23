package api

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Directory browsing for the library folder picker. Only directories are
// listed, hidden entries (dot-prefixed) are filtered out, and browsing is
// confined to the server's fs root (YINGKU_FS_ROOT, default "/").

type fsDirDTO struct {
	Name string `json:"Name"`
	Path string `json:"Path"`
}

type fsListDTO struct {
	Path   string     `json:"Path"`
	Parent string     `json:"Parent,omitempty"`
	Dirs   []fsDirDTO `json:"Dirs"`
}

// withinRoot reports whether clean lies inside root (or equals it).
func withinRoot(root, clean string) bool {
	if clean == root {
		return true
	}
	rel, err := filepath.Rel(root, clean)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Server) handleFSList(w http.ResponseWriter, r *http.Request) {
	root := s.fsRoot
	target := r.URL.Query().Get("path")
	if target == "" {
		target = root
	}
	if !filepath.IsAbs(target) {
		writeErr(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	clean := filepath.Clean(target)
	if !withinRoot(root, clean) {
		writeErr(w, http.StatusBadRequest, "path outside allowed root")
		return
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, "directory not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := fsListDTO{Path: clean, Dirs: []fsDirDTO{}}
	if clean != root {
		parent := filepath.Dir(clean)
		if withinRoot(root, parent) {
			out.Parent = parent
		}
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out.Dirs = append(out.Dirs, fsDirDTO{
			Name: e.Name(),
			Path: filepath.Join(clean, e.Name()),
		})
	}
	sort.Slice(out.Dirs, func(i, j int) bool { return out.Dirs[i].Name < out.Dirs[j].Name })
	writeJSON(w, http.StatusOK, out)
}
