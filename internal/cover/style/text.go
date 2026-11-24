package style

import (
	"image"
	"image/color"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"

	"yingku/internal/cover/fonts"
)

// TextKit draws text on an RGBA canvas with PIL-compatible anchoring.
//
// PIL's default text anchor is "la": the (x, y) passed to draw.text is the
// left edge of the advance and the top of the ascender line. Go's
// font.Drawer takes a baseline origin, so Draw converts y by the face
// ascent. DrawInk instead positions the ink (bounding) box top-left exactly
// at (x, y), matching the Python styles that compensate with
// textbbox offsets (draw.text((x - bbox[0], y - bbox[1]), ...)).
type TextKit struct {
	Img *image.RGBA
}

// Face loads a face at size (pixels), like PIL ImageFont.truetype.
func Face(path string, size float64) (font.Face, error) {
	return fonts.NewFace(path, size)
}

// AdvanceWidth mirrors PIL draw.textlength.
func AdvanceWidth(face font.Face, text string) float64 {
	return f64(font.MeasureString(face, text))
}

// InkBox returns the ink bounding box of text relative to the baseline
// origin: (minX, minY, width, height), mirroring PIL textbbox((0,0), ...)
// shifted so min is relative to the anchor point.
func InkBox(face font.Face, text string) (minX, minY, w, h float64) {
	b, _ := font.BoundString(face, text)
	minX = f64(b.Min.X)
	minY = f64(b.Min.Y)
	w = f64(b.Max.X - b.Min.X)
	h = f64(b.Max.Y - b.Min.Y)
	return
}

// InkHeight mirrors Python _text_height: bbox[3] - bbox[1].
func InkHeight(face font.Face, text string) int {
	_, _, _, h := InkBox(face, text)
	return int(math.Round(h))
}

// InkWidth mirrors bbox[2] - bbox[0].
func InkWidth(face font.Face, text string) int {
	_, _, w, _ := InkBox(face, text)
	return int(math.Round(w))
}

func f64(v fixed.Int26_6) float64 { return float64(v) / 64 }

func (tk *TextKit) drawDot(face font.Face, dotX, dotY float64, text string, c color.RGBA) {
	d := &font.Drawer{
		Dst:  tk.Img,
		Src:  image.NewUniform(c),
		Face: face,
		Dot: fixed.Point26_6{
			X: fixed.Int26_6(dotX * 64),
			Y: fixed.Int26_6(dotY * 64),
		},
	}
	d.DrawString(text)
}

// Draw renders text with PIL "la" anchoring at (x, y).
func (tk *TextKit) Draw(face font.Face, x, y float64, text string, c color.RGBA) {
	ascent := f64(face.Metrics().Ascent)
	tk.drawDot(face, x, y+ascent, text, c)
}

// DrawInk renders text so its ink box top-left lands exactly at (x, y).
func (tk *TextKit) DrawInk(face font.Face, x, y float64, text string, c color.RGBA) {
	minX, minY, _, _ := InkBox(face, text)
	tk.drawDot(face, x-minX, y-minY, text, c)
}

// DrawTracked mirrors the Python _draw_tracked helpers: per-rune drawing,
// advancing by the rune's advance width plus tracking.
func (tk *TextKit) DrawTracked(face font.Face, x, y float64, text string, c color.RGBA, tracking float64) {
	ascent := f64(face.Metrics().Ascent)
	cur := x
	for _, r := range text {
		s := string(r)
		tk.drawDot(face, cur, y+ascent, s, c)
		cur += AdvanceWidth(face, s) + tracking
	}
}

// DrawStroke draws text with a PIL-style stroke: the stroke color is stamped
// at every offset within strokeWidth, then the fill on top. Anchoring is
// PIL "la" like Draw.
func (tk *TextKit) DrawStroke(face font.Face, x, y float64, text string, fill, stroke color.RGBA, strokeWidth int) {
	if strokeWidth > 0 {
		for dy := -strokeWidth; dy <= strokeWidth; dy++ {
			for dx := -strokeWidth; dx <= strokeWidth; dx++ {
				if dx == 0 && dy == 0 {
					continue
				}
				if dx*dx+dy*dy <= strokeWidth*strokeWidth {
					tk.Draw(face, x+float64(dx), y+float64(dy), text, stroke)
				}
			}
		}
	}
	tk.Draw(face, x, y, text, fill)
}

// FitFontAdvance mirrors the _fit_font used by cinematic/editorial/retro:
// shrink by x0.92 (floor 16) until the advance width fits maxWidth.
func FitFontAdvance(path, text string, startSize float64, maxWidth int) (font.Face, error) {
	size := int(startSize)
	if size < 16 {
		size = 16
	}
	for size > 16 {
		face, err := Face(path, float64(size))
		if err != nil {
			return nil, err
		}
		if AdvanceWidth(face, text) <= float64(maxWidth) {
			return face, nil
		}
		size = int(float64(size) * 0.92)
	}
	return Face(path, float64(size))
}

// FitFontInk mirrors the _fit_font used by editorial_big/noir: shrink in
// steps of 4 (floor 24) until the ink-box width fits maxWidth.
func FitFontInk(path, text string, startSize float64, maxWidth int) (font.Face, error) {
	current := int(startSize)
	if current < 24 {
		current = 24
	}
	for current > 24 {
		face, err := Face(path, float64(current))
		if err != nil {
			return nil, err
		}
		if InkWidth(face, text) <= maxWidth {
			return face, nil
		}
		current -= 4
	}
	return Face(path, 24)
}
