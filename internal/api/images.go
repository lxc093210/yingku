package api

import (
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // register PNG decoding for image.Decode
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// ---- per-key mutual exclusion ----

// keyedLocks serializes work per string key (one thumbnail generation
// or one TMDB art fetch per item+kind at a time) without blocking
// unrelated keys. Locks are created on demand and kept for the life of
// the process; the key space (item ids × kinds × widths) is bounded by
// what clients actually request.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{locks: map[string]*sync.Mutex{}}
}

// lock acquires the mutex for key and returns its unlock function.
func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// ---- cache locations (all under <data dir>/cache) ----

func (s *Server) cacheDir() string {
	return filepath.Join(filepath.Dir(s.st.DBPath()), "cache")
}

// thumbCacheFile is the scaled-JPEG cache path for one item image.
func (s *Server) thumbCacheFile(id int64, kind string, w int) string {
	return filepath.Join(s.cacheDir(), "thumbs",
		fmt.Sprintf("%d-%s-%d.jpg", id, kind, w))
}

// tmdbArtFile is the cached TMDB-downloaded artwork for one item image.
func (s *Server) tmdbArtFile(id int64, kind string) string {
	return filepath.Join(s.cacheDir(), "tmdb-art",
		fmt.Sprintf("%d-%s.jpg", id, kind))
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// parseImageWidth extracts the ?w= resize parameter. 0 means "no valid
// width requested" (missing, unparsable, or outside 40–1280) and the
// caller serves the original image, exactly as before ?w= existed.
func parseImageWidth(r *http.Request) int {
	v := r.URL.Query().Get("w")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 40 || n > 1280 {
		return 0
	}
	return n
}

// resolveImageFile finds the on-disk file to serve for an item image:
// the scanner-registered local file first, then a previously cached
// TMDB download, then — for movies/series with a TMDB id — a lazy TMDB
// fetch cached under cache/tmdb-art. "" when nothing is available.
func (s *Server) resolveImageFile(id int64, kind string) string {
	if p, err := s.st.ImagePath(id, kind); err == nil && p != "" {
		clean := filepath.Clean(p)
		if allowed, aerr := s.pathWithinRoots(clean); aerr == nil && allowed && fileExists(clean) {
			return clean
		}
	}
	if cache := s.tmdbArtFile(id, kind); fileExists(cache) {
		return cache
	}
	return s.lazyTMDBArt(id, kind)
}

// lazyTMDBArt lazily downloads the requested artwork kind from TMDB
// (poster at w780, backdrop at w1280) into the tmdb-art cache and
// returns its path. Concurrent calls for the same item+kind collapse
// into one fetch. Any failure returns "" and caches nothing, so a
// later request may retry.
func (s *Server) lazyTMDBArt(id int64, kind string) string {
	it, err := s.st.GetItem(id)
	if err != nil || it == nil || it.TmdbID <= 0 {
		return ""
	}
	if it.Type != "movie" && it.Type != "series" {
		return ""
	}
	cfg, err := s.tmdbCfg()
	if err != nil || !cfg.available() {
		return ""
	}
	unlock := s.imgLocks.lock(fmt.Sprintf("art-%d-%s", id, kind))
	defer unlock()
	// Another goroutine may have fetched it while we waited.
	if cache := s.tmdbArtFile(id, kind); fileExists(cache) {
		return cache
	}
	tmdbKind := "movie"
	if it.Type == "series" {
		tmdbKind = "tv"
	}
	poster, backdrop, err := fetchTMDBArt(cfg, tmdbKind, it.TmdbID)
	if err != nil {
		return ""
	}
	filePath, size := poster, "w780"
	if kind == "backdrop" {
		filePath, size = backdrop, "w1280"
	}
	if filePath == "" {
		return ""
	}
	data, err := cfg.downloadSized(filePath, size)
	if err != nil || len(data) == 0 {
		return ""
	}
	cache := s.tmdbArtFile(id, kind)
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		return ""
	}
	tmp := cache + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return ""
	}
	if err := os.Rename(tmp, cache); err != nil {
		return ""
	}
	return cache
}

// serveItemImage serves one item poster/backdrop: the resolved source
// file as-is, or — with a valid ?w= — a cached JPEG scaled to that
// width (generated once, then served from cache/thumbs).
func (s *Server) serveItemImage(w http.ResponseWriter, r *http.Request, id int64, kind string) {
	src := s.resolveImageFile(id, kind)
	if src == "" {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	width := parseImageWidth(r)
	if width == 0 {
		serveFile(w, r, src)
		return
	}
	cache := s.thumbCacheFile(id, kind, width)
	if fileExists(cache) {
		serveFile(w, r, cache)
		return
	}
	unlock := s.imgLocks.lock(fmt.Sprintf("thumb-%d-%s-%d", id, kind, width))
	defer unlock()
	if fileExists(cache) {
		serveFile(w, r, cache)
		return
	}
	if ok := makeThumb(src, cache, width); ok {
		serveFile(w, r, cache)
		return
	}
	// Source not wider than requested, or undecodable: the original
	// file is the best answer (and the pre-?w= behavior).
	serveFile(w, r, src)
}

// thumbBG is the matte color transparent artwork is composited onto.
var thumbBG = color.RGBA{R: 0x11, G: 0x11, B: 0x11, A: 0xff}

// makeThumb decodes src, scales it to width w (never upscales), and
// writes it as JPEG q80 to dst. It reports false when no thumbnail was
// produced: the source is not wider than w, or it cannot be
// decoded/encoded — the caller then serves the original.
func makeThumb(src, dst string, w int) bool {
	f, err := os.Open(src)
	if err != nil {
		return false
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return false
	}
	out := scaleToWidth(img, w)
	if out == nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false
	}
	tmp := dst + ".tmp"
	of, err := os.Create(tmp)
	if err != nil {
		return false
	}
	encErr := jpeg.Encode(of, out, &jpeg.Options{Quality: 80})
	closeErr := of.Close()
	if encErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// scaleToWidth downscales img to width w with area-weighted sampling
// (each destination pixel averages the source rectangle it covers,
// which stays clean at the large ratios poster thumbnails need) and
// composites the result over thumbBG. It returns nil when the source
// is not wider than w — thumbnails never upscale. Pure stdlib: the
// x/image draw filters were dropped because recent x/image releases
// force a newer Go directive than this module declares.
func scaleToWidth(img image.Image, w int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= w || sh <= 0 {
		return nil
	}
	th := sh * w / sw
	if th < 1 {
		th = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, w, th))
	xScale := float64(sw) / float64(w)
	yScale := float64(sh) / float64(th)
	for dy := 0; dy < th; dy++ {
		y0 := float64(dy) * yScale
		y1 := y0 + yScale
		sy0 := int(y0)
		sy1 := int(math.Ceil(y1))
		if sy1 > sh {
			sy1 = sh
		}
		for dx := 0; dx < w; dx++ {
			x0 := float64(dx) * xScale
			x1 := x0 + xScale
			sx0 := int(x0)
			sx1 := int(math.Ceil(x1))
			if sx1 > sw {
				sx1 = sw
			}
			var rSum, gSum, bSum, aSum, wSum float64
			for sy := sy0; sy < sy1; sy++ {
				wy := math.Min(y1, float64(sy+1)) - math.Max(y0, float64(sy))
				if wy <= 0 {
					continue
				}
				for sx := sx0; sx < sx1; sx++ {
					wx := math.Min(x1, float64(sx+1)) - math.Max(x0, float64(sx))
					if wx <= 0 {
						continue
					}
					weight := wx * wy
					c := color.NRGBAModel.Convert(img.At(b.Min.X+sx, b.Min.Y+sy)).(color.NRGBA)
					a := float64(c.A) / 255
					rSum += float64(c.R) * a * weight
					gSum += float64(c.G) * a * weight
					bSum += float64(c.B) * a * weight
					aSum += a * weight
					wSum += weight
				}
			}
			px := thumbBG
			if aSum > 0 && wSum > 0 {
				alpha := aSum / wSum // mean coverage 0..1
				blend := func(sum float64) uint8 {
					v := (sum/aSum)*alpha + float64(0x11)*(1-alpha)
					if v > 255 {
						v = 255
					}
					return uint8(v + 0.5)
				}
				px = color.RGBA{R: blend(rSum), G: blend(gSum), B: blend(bSum), A: 0xff}
			}
			out.SetRGBA(dx, dy, px)
		}
	}
	return out
}
