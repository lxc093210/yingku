package api

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"yingku/internal/store"
)

// Library covers for Emby clients: instead of one arbitrary newest
// item's backdrop (often a dull still from a random new import), the
// cover is a collage of the library's best-regarded backdrops — the
// trusted rating band first, high-resolution art first — composed
// into a single 1920x1080 JPEG and cached on disk. The DTO image tag
// carries a signature of the picked items, so clients refetch when
// the pick changes.

func (s *Server) libraryArtDir() string {
	return filepath.Join(s.cacheDir(), "library-art")
}

// libraryArtSignature fingerprints the picked item set.
func libraryArtSignature(items []*store.Item) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = fmt.Sprint(it.ID)
	}
	sum := sha1.Sum([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])[:10]
}

// libraryCover returns the cover image path for a library and the
// pick signature. A single-pick library serves that item's backdrop
// directly; two or more picks are composed into a collage.
func (s *Server) libraryCover(libID int64) (string, string) {
	items, err := s.st.LibraryArtworkItems(libID, 4)
	if err != nil || len(items) == 0 {
		return "", ""
	}
	sig := libraryArtSignature(items)
	if len(items) == 1 {
		return s.resolveImageFile(items[0].ID, "backdrop"), sig
	}
	dst := filepath.Join(s.libraryArtDir(), fmt.Sprintf("%d-%s.jpg", libID, sig))
	if fileExists(dst) {
		return dst, sig
	}
	unlock := s.imgLocks.lock(fmt.Sprintf("libart-%d", libID))
	defer unlock()
	if fileExists(dst) {
		return dst, sig
	}
	if !s.buildLibraryCollage(items, dst) {
		// Collage failed (undecodable sources): fall back to the
		// best single backdrop rather than no cover at all.
		return s.resolveImageFile(items[0].ID, "backdrop"), sig
	}
	// Drop collages of older picks for this library.
	old, _ := filepath.Glob(filepath.Join(s.libraryArtDir(), fmt.Sprintf("%d-*.jpg", libID)))
	for _, f := range old {
		if f != dst {
			_ = os.Remove(f)
		}
	}
	return dst, sig
}

// buildLibraryCollage composes the picked backdrops into dst.
// Layouts: 2 picks = two horizontal strips; 3 = one strip over two
// cells; 4 = a 2x2 grid. Cells whose source fails to decode stay
// matte; false is returned only when every cell failed.
func (s *Server) buildLibraryCollage(items []*store.Item, dst string) bool {
	const W, H = 1920, 1080
	canvas := image.NewRGBA(image.Rect(0, 0, W, H))
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			canvas.Set(x, y, thumbBG)
		}
	}
	type cell struct{ x, y, w, h int }
	var cells []cell
	switch {
	case len(items) == 2:
		cells = []cell{{0, 0, W, H / 2}, {0, H / 2, W, H / 2}}
	case len(items) == 3:
		cells = []cell{{0, 0, W, H / 2}, {0, H / 2, W / 2, H / 2}, {W / 2, H / 2, W / 2, H / 2}}
	default:
		cells = []cell{{0, 0, W / 2, H / 2}, {W / 2, 0, W / 2, H / 2},
			{0, H / 2, W / 2, H / 2}, {W / 2, H / 2, W / 2, H / 2}}
	}
	filled := 0
	for i, c := range cells {
		if i >= len(items) {
			break
		}
		src := s.resolveImageFile(items[i].ID, "backdrop")
		if src == "" {
			continue
		}
		f, err := os.Open(src)
		if err != nil {
			continue
		}
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			continue
		}
		drawFill(canvas, img, c.x, c.y, c.w, c.h)
		filled++
	}
	if filled == 0 {
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
	encErr := jpeg.Encode(of, canvas, &jpeg.Options{Quality: 85})
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

// drawFill paints src into the (x, y, w, h) box of dst, center-cropped
// to the box's aspect (cover-fit), with the same area-weighted
// sampling scaleToWidth uses.
func drawFill(dst *image.RGBA, src image.Image, x, y, w, h int) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return
	}
	// Centered source rectangle with the box's aspect ratio.
	var cx, cy, cw, ch int
	if sw*h > sh*w { // source wider: crop the sides
		cw = sh * w / h
		ch = sh
		cx = (sw - cw) / 2
		cy = 0
	} else { // source taller: crop top/bottom
		cw = sw
		ch = sw * h / w
		cx = 0
		cy = (sh - ch) / 2
	}
	if cw < 1 {
		cw = 1
	}
	if ch < 1 {
		ch = 1
	}
	xScale := float64(cw) / float64(w)
	yScale := float64(ch) / float64(h)
	for dy := 0; dy < h; dy++ {
		sy0f := float64(cy) + float64(dy)*yScale
		sy1f := sy0f + yScale
		sy0 := int(sy0f)
		sy1 := int(sy1f + 0.999999)
		if sy1 > cy+ch {
			sy1 = cy + ch
		}
		for dx := 0; dx < w; dx++ {
			sx0f := float64(cx) + float64(dx)*xScale
			sx1f := sx0f + xScale
			sx0 := int(sx0f)
			sx1 := int(sx1f + 0.999999)
			if sx1 > cx+cw {
				sx1 = cx + cw
			}
			var rSum, gSum, bSum, wSum float64
			for sy := sy0; sy < sy1; sy++ {
				wy := minF(sy1f, float64(sy+1)) - maxF(sy0f, float64(sy))
				if wy <= 0 {
					continue
				}
				for sx := sx0; sx < sx1; sx++ {
					wx := minF(sx1f, float64(sx+1)) - maxF(sx0f, float64(sx))
					if wx <= 0 {
						continue
					}
					r, g, bl, _ := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					weight := wx * wy
					rSum += float64(r>>8) * weight
					gSum += float64(g>>8) * weight
					bSum += float64(bl>>8) * weight
					wSum += weight
				}
			}
			if wSum == 0 {
				continue
			}
			dst.Set(x+dx, y+dy, rgba(rSum/wSum, gSum/wSum, bSum/wSum))
		}
	}
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func rgba(r, g, b float64) color.RGBA {
	return color.RGBA{R: uint8(r + 0.5), G: uint8(g + 0.5), B: uint8(b + 0.5), A: 0xff}
}

// serveLibraryImage serves the library cover, scaled to width when
// the client asked for a smaller rendition.
func (s *Server) serveLibraryImage(w http.ResponseWriter, r *http.Request, libID int64, width int) {
	path, sig := s.libraryCover(libID)
	if path == "" {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	if width > 0 && width < 1920 {
		thumb := filepath.Join(s.libraryArtDir(), fmt.Sprintf("thumb-%d-%s-%d.jpg", libID, sig, width))
		if fileExists(thumb) {
			serveFile(w, r, thumb)
			return
		}
		if ok := makeThumb(path, thumb, width); ok {
			serveFile(w, r, thumb)
			return
		}
	}
	serveFile(w, r, path)
}
