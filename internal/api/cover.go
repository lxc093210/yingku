package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"

	"yingku/internal/cover"
	"yingku/internal/cover/enhance"
	"yingku/internal/cover/imgx"
	"yingku/internal/store"
)

// Cover generation (batch 28): the mediacover-go renderer, integrated
// as a yingku module. Candidates come straight from the store (items
// with a local backdrop), are scored with the same technical scoring
// the standalone generator applies to its maxWidth=480 renditions,
// and the rendered 1920x1080 PNG lands at the uploaded-cover location
// (cache/library-art/upload-{libID}.png), so libraryCover() picks it
// up on every surface — web, Emby clients, Nan — with no extra wiring.

const (
	settingCoverStyle      = "cover_style"
	settingCoverSort       = "cover_sort"
	settingCoverLastRun    = "cover_last_run"
	settingCoverLastResult = "cover_last_result"
)

func (s *Server) coverStyleSort() (styleName, sortBy string) {
	styleName, _ = s.st.GetSetting(settingCoverStyle)
	if !cover.IsStyle(styleName) {
		styleName = "editorial"
	}
	sortBy, _ = s.st.GetSetting(settingCoverSort)
	if sortBy != "rating" {
		sortBy = "datecreated"
	}
	return styleName, sortBy
}

func (s *Server) coverLastRun() (int64, string) {
	raw, _ := s.st.GetSetting(settingCoverLastRun)
	n, _ := strconv.ParseInt(raw, 10, 64)
	res, _ := s.st.GetSetting(settingCoverLastResult)
	return n, res
}

func (s *Server) recordCoverRun(result string) {
	_ = s.st.SetSetting(settingCoverLastRun, strconv.FormatInt(time.Now().Unix(), 10))
	_ = s.st.SetSetting(settingCoverLastResult, result)
}

// runCoverGeneration generates covers for all libraries (or one),
// guarded by the shared "cover" run lock, and records the outcome
// both in the cover settings (for GET /api/cover) and as the return
// value (for the covers job's lastResult).
func (s *Server) runCoverGeneration(onlyLib int64) string {
	if !s.tryAcquire("cover") {
		return "封面生成已在运行，本轮跳过"
	}
	defer s.release("cover")
	result := s.generateCovers(onlyLib)
	s.recordCoverRun(result)
	log.Printf("cover generation: %s", result)
	return result
}

func (s *Server) generateCovers(onlyLib int64) string {
	styleName, sortBy := s.coverStyleSort()
	required := cover.RequiredItems(styleName)
	poolSize := required * 4
	if poolSize < 12 {
		poolSize = 12
	}
	zh, en, serif, err := cover.FontPaths(filepath.Join(s.cacheDir(), "cover-fonts"))
	if err != nil {
		return "字体准备失败：" + err.Error()
	}
	libs, err := s.st.ListLibraries()
	if err != nil {
		return "读取媒体库失败：" + err.Error()
	}
	okCount := 0
	var failed []string
	total := 0
	for _, lib := range libs {
		if onlyLib > 0 && lib.ID != onlyLib {
			continue
		}
		total++
		if err := s.generateCoverForLibrary(lib, styleName, sortBy, required, poolSize, zh, en, serif); err != nil {
			failed = append(failed, fmt.Sprintf("%s（%v）", lib.Name, err))
			continue
		}
		okCount++
	}
	result := fmt.Sprintf("生成 %d/%d 个库封面（风格 %s）", okCount, total, styleName)
	if len(failed) > 0 {
		result += "；失败：" + strings.Join(failed, "、")
	}
	return result
}

func (s *Server) generateCoverForLibrary(lib store.Library, styleName, sortBy string, required, poolSize int, zhFont, enFont, serifFont string) error {
	items, err := s.st.CoverCandidates(lib.ID, sortBy, poolSize)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("无带 backdrop 的条目")
	}
	// Score the local backdrop files (at a 480-wide rendition, like
	// the standalone generator scores its downloads), best first.
	type cand struct {
		path  string
		score float64
	}
	var cands []cand
	for _, it := range items {
		p := s.localImageFile(it.ID, "backdrop")
		if p == "" {
			continue
		}
		cands = append(cands, cand{path: p, score: scoreBackdropFile(p)})
	}
	if len(cands) == 0 {
		return fmt.Errorf("候选 backdrop 文件不可读")
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	picks := cands
	if len(picks) > required {
		picks = picks[:required]
	}
	dir, err := os.MkdirTemp("", "yk-cover-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for i := 0; i < required; i++ {
		src := picks[i%len(picks)].path // cycle-fill missing slots
		if err := copyFileBytes(src, filepath.Join(dir, fmt.Sprintf("%d.jpg", i+1))); err != nil {
			return err
		}
	}
	params := cover.Params(lib.Name, coverEnTitle(lib.Kind), zhFont, enFont, serifFont)
	png, err := cover.Render(styleName, dir, params)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.libraryArtDir(), 0o755); err != nil {
		return err
	}
	dst := filepath.Join(s.libraryArtDir(), fmt.Sprintf("upload-%d.png", lib.ID))
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, png, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// coverEnTitle maps a library kind to the English cover title, the
// same MOVIES / TV SHOWS convention as the web UI's library cards.
func coverEnTitle(kind string) string {
	switch kind {
	case "movie":
		return "MOVIES"
	case "series":
		return "TV SHOWS"
	}
	return "COLLECTION"
}

// scoreBackdropFile scores one local backdrop file the way the
// standalone generator scores its maxWidth=480 downloads: decode,
// downscale to 480 wide when larger, then the enhance technical
// score. Undecodable files score -inf and sink to the pool's end.
func scoreBackdropFile(path string) float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return enhance.ScoreBackdropBytes(nil)
	}
	img, err := imgx.DecodeBytes(data)
	if err != nil {
		return enhance.ScoreBackdropBytes(nil)
	}
	b := img.Bounds()
	if w := b.Dx(); w > 480 && b.Dy() > 0 {
		h := 480 * b.Dy() / w
		if h < 1 {
			h = 1
		}
		return enhance.ScoreImage(imgx.Resize(img, 480, h, xdraw.CatmullRom))
	}
	return enhance.ScoreImage(img)
}

func copyFileBytes(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- HTTP API ----

type coverLibDTO struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	HasCover bool   `json:"hasCover"`
}

type coverDTO struct {
	Style      string        `json:"style"`
	Sort       string        `json:"sort"`
	Styles     []string      `json:"styles"`
	Running    bool          `json:"running"`
	LastRun    int64         `json:"lastRun"`
	LastResult string        `json:"lastResult"`
	Libraries  []coverLibDTO `json:"libraries"`
}

func (s *Server) coverDTO() (coverDTO, error) {
	styleName, sortBy := s.coverStyleSort()
	lastRun, lastResult := s.coverLastRun()
	libs, err := s.st.ListLibraries()
	if err != nil {
		return coverDTO{}, err
	}
	d := coverDTO{
		Style: styleName, Sort: sortBy, Styles: cover.Styles,
		Running: s.isRunning("cover"), LastRun: lastRun, LastResult: lastResult,
		Libraries: []coverLibDTO{},
	}
	for _, lib := range libs {
		d.Libraries = append(d.Libraries, coverLibDTO{
			ID: lib.ID, Name: lib.Name, HasCover: s.libraryUploadFile(lib.ID) != "",
		})
	}
	return d, nil
}

func (s *Server) handleCoverGet(w http.ResponseWriter, r *http.Request) {
	d, err := s.coverDTO()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleCoverPut(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Style *string `json:"style"`
		Sort  *string `json:"sort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.Style != nil {
		if !cover.IsStyle(*p.Style) {
			writeErr(w, http.StatusBadRequest, "unknown cover style")
			return
		}
		if err := s.st.SetSetting(settingCoverStyle, *p.Style); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if p.Sort != nil {
		if *p.Sort != "datecreated" && *p.Sort != "rating" {
			writeErr(w, http.StatusBadRequest, "unknown cover sort")
			return
		}
		if err := s.st.SetSetting(settingCoverSort, *p.Sort); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.handleCoverGet(w, r)
}

func (s *Server) handleCoverGenerate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		LibraryID int64 `json:"libraryId"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&p)
	}
	if s.isRunning("cover") {
		writeErr(w, http.StatusConflict, "cover generation already running")
		return
	}
	go s.runCoverGeneration(p.LibraryID)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}
