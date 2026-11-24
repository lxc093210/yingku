package style

import (
	"image"
	"image/color"
	"math"
)

// FillRectIncl fills the inclusive rectangle [x0,y0]-[x1,y1], matching PIL
// ImageDraw.rectangle which includes both endpoints.
func FillRectIncl(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	r := image.Rect(x0, y0, x1+1, y1+1).Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetRGBA(x, y, c)
		}
	}
}

// StrokeRectIncl draws a PIL-style rectangle outline: the stroke grows
// inward from the inclusive rectangle edges.
func StrokeRectIncl(img *image.RGBA, x0, y0, x1, y1, width int, c color.RGBA) {
	if width <= 0 {
		width = 1
	}
	FillRectIncl(img, x0, y0, x1, y0+width-1, c)
	FillRectIncl(img, x0, y1-width+1, x1, y1, c)
	FillRectIncl(img, x0, y0, x0+width-1, y1, c)
	FillRectIncl(img, x1-width+1, y0, x1, y1, c)
}

// HLine draws a horizontal PIL line of the given width starting at y.
func HLine(img *image.RGBA, x0, x1, y, width int, c color.RGBA) {
	if width <= 0 {
		width = 1
	}
	FillRectIncl(img, x0, y, x1, y+width-1, c)
}

// FillRoundedRect fills a rounded rectangle (film sprocket holes in retro).
func FillRoundedRect(img *image.RGBA, x0, y0, x1, y1, radius int, c color.RGBA) {
	r := float64(radius)
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			if x < img.Bounds().Min.X || x >= img.Bounds().Max.X || y < img.Bounds().Min.Y || y >= img.Bounds().Max.Y {
				continue
			}
			cx := math.Max(math.Max(float64(x0)+r-float64(x), float64(x)-(float64(x1)-r)), 0)
			cy := math.Max(math.Max(float64(y0)+r-float64(y), float64(y)-(float64(y1)-r)), 0)
			if math.Hypot(cx, cy) <= r {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

// FillRectInclNRGBA is FillRectIncl for straight-alpha canvases (shadow layers).
func FillRectInclNRGBA(img *image.NRGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	r := image.Rect(x0, y0, x1+1, y1+1).Intersect(img.Bounds())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}
