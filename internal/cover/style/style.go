// Package style ports the five custom cover styles from the Python
// mediacovergenerator.style package. Each render returns the finished RGBA
// canvas (with transparent rounded corners); PNG encoding and base64 are
// the caller's job, mirroring how the Python styles return base64 PNG text.
package style

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"

	"yingku/internal/cover/imgx"
)

// Params carries everything generator.render passes to a style callable.
type Params struct {
	TitleZh string
	TitleEn string

	ZhFontPath string
	EnFontPath string
	// SerifFontPath is the yasong serif used by the editorial styles
	// (already resolved with fallback to the zh font).
	SerifFontPath string

	FontSizeZh float64
	FontSizeEn float64

	Width  int
	Height int
}

// CornerRadius is the shared rounded-corner radius: 5.2% of the short side.
func (p Params) CornerRadius() int {
	m := p.Width
	if p.Height < m {
		m = p.Height
	}
	return int(float64(m) * 0.052)
}

var (
	white     = color.RGBA{255, 255, 255, 255}
	black     = color.RGBA{0, 0, 0, 255}
	paperCol  = color.RGBA{243, 239, 231, 255}
	inkEditor = color.RGBA{24, 24, 24, 255}
	inkBig    = color.RGBA{26, 24, 22, 255}
)

// LoadImageFile decodes an image file (the downloaded backdrop cache files
// are named *.jpg but hold whatever bytes the server sent).
func LoadImageFile(path string) (*image.RGBA, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	img, err := imgx.DecodeBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return imgx.ToRGBA(img), nil
}

// NumberedImage loads "<dir>/<n>.jpg" like the Python multi-image styles.
func NumberedImage(dir string, n int) (*image.RGBA, error) {
	return LoadImageFile(filepath.Join(dir, fmt.Sprintf("%d.jpg", n)))
}

// RequiredItems mirrors generator.get_required_items.
func RequiredItems(styleName string) int {
	switch styleName {
	case "static_3":
		return 9
	case "editorial", "editorial_big":
		return 3
	default:
		return 1
	}
}

// IsSingleImageStyle mirrors generator.is_single_image_style.
func IsSingleImageStyle(styleName string) bool {
	switch styleName {
	case "static_1", "static_2", "cinematic", "noir", "retro":
		return true
	}
	return false
}

// IsBackdropFirst mirrors service.BACKDROP_FIRST_STYLES.
func IsBackdropFirst(styleName string) bool {
	switch styleName {
	case "cinematic", "editorial", "editorial_big", "noir", "retro":
		return true
	}
	return false
}

// IsPorted reports whether this Go build can render the style. The three
// upstream static styles are not ported (see README).
func IsPorted(styleName string) bool {
	switch styleName {
	case "cinematic", "editorial", "editorial_big", "noir", "retro":
		return true
	}
	return false
}
