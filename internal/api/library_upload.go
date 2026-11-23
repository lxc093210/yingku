package api

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Library cover overrides uploaded through the Emby image API —
// this is how MediaCoverGenerator publishes the covers it renders:
// it lists libraries via /Library/VirtualFolders/Query, samples item
// artwork through the regular item/image endpoints, and POSTs the
// finished cover (base64 body, Emby convention) to
// /Items/{libraryId}/Images/Primary. An uploaded cover wins over the
// built-in collage in every client view.

// libraryUploadFile returns the uploaded cover file for a library,
// or "" when the library has no override.
func (s *Server) libraryUploadFile(libID int64) string {
	matches, _ := filepath.Glob(filepath.Join(s.libraryArtDir(), fmt.Sprintf("upload-%d.*", libID)))
	for _, m := range matches {
		switch strings.ToLower(filepath.Ext(m)) {
		case ".jpg", ".png", ".webp", ".gif":
			return m
		}
	}
	return ""
}

// libraryUploadSig fingerprints the current upload (mtime + size) so
// the DTO image tag changes whenever a new cover is posted and
// clients refetch instead of showing the stale cover forever.
func (s *Server) libraryUploadSig(libID int64) string {
	p := s.libraryUploadFile(libID)
	if p == "" {
		return ""
	}
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("up%d-%d", fi.ModTime().Unix(), fi.Size())
}

// handleVirtualFoldersQuery serves GET /Library/VirtualFolders/Query
// in the shape Emby returns and MediaCoverGenerator parses:
// {Items: [{Id, Name, CollectionType}]}.
func (s *Server) handleVirtualFoldersQuery(w http.ResponseWriter, r *http.Request) {
	libs, err := s.st.ListLibraries()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type vf struct {
		Name           string   `json:"Name"`
		Id             string   `json:"Id"`
		ItemId         string   `json:"ItemId"`
		CollectionType string   `json:"CollectionType"`
		Locations      []string `json:"Locations"`
	}
	items := make([]vf, 0, len(libs))
	for _, l := range libs {
		ct := map[string]string{"movie": "movies", "series": "tvshows", "mixed": "mixed"}[l.Kind]
		items = append(items, vf{
			Name:           l.Name,
			Id:             embyLibraryID(l.ID),
			ItemId:         embyLibraryID(l.ID),
			CollectionType: ct,
			Locations:      l.Paths,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": items})
}

// sniffImageExt identifies an image payload by its magic bytes.
func sniffImageExt(data []byte) string {
	switch {
	case len(data) >= 4 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G':
		return "png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "jpg"
	case len(data) >= 12 && string(data[8:12]) == "WEBP":
		return "webp"
	case len(data) >= 4 && string(data[:4]) == "GIF8":
		return "gif"
	}
	return ""
}

// handleEmbyLibraryImageUpload serves POST /Items/{id}/Images/Primary
// for library (CollectionFolder) ids. The body is the base64-encoded
// image, per Emby's upload convention. The decoded image becomes the
// library's cover override.
func (s *Server) handleEmbyLibraryImageUpload(w http.ResponseWriter, r *http.Request) {
	raw, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || raw < embyLibraryIDOffset {
		writeErr(w, http.StatusNotFound, "not a library id")
		return
	}
	libID := raw - embyLibraryIDOffset
	lib, err := s.st.GetLibrary(libID)
	if err != nil || lib == nil {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "data:") {
		if i := strings.Index(text, ","); i >= 0 {
			text = text[i+1:]
		}
	}
	var data []byte
	if ext := sniffImageExt(body); ext != "" {
		// Tolerate a raw (non-base64) image body too.
		data = body
	} else {
		dec, derr := base64.StdEncoding.DecodeString(text)
		if derr != nil {
			dec, derr = io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(text)))
		}
		if derr != nil || len(dec) == 0 {
			writeErr(w, http.StatusBadRequest, "body is not base64 image data")
			return
		}
		data = dec
	}
	ext := sniffImageExt(data)
	if ext == "" {
		writeErr(w, http.StatusBadRequest, "unsupported image type")
		return
	}
	dir := s.libraryArtDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// One override per library: drop the previous upload and any
	// scaled thumbs derived from older covers.
	old, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("upload-%d.*", libID)))
	for _, f := range old {
		_ = os.Remove(f)
	}
	thumbs, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("thumb-%d-*.jpg", libID)))
	for _, f := range thumbs {
		_ = os.Remove(f)
	}
	dst := filepath.Join(dir, fmt.Sprintf("upload-%d.%s", libID, ext))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
