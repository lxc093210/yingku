// Package fonts provides opentype faces for the cover styles.
//
// Trimmed port of mediacover-go's fonts package: yingku embeds its two
// production fonts (chaohei for zh titles, yasong for en titles and the
// editorial serif) in the binary, so the config-driven resolver and
// downloader of the standalone generator are not needed here. Only
// parsing, validation and face creation remain.
package fonts

import (
	"os"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// ValidateFontFile reports whether path is an existing, non-empty,
// parseable TTF/OTF font file.
func ValidateFontFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return false
	}
	ext := strings.ToLower(path)
	if strings.HasSuffix(ext, ".ttf") || strings.HasSuffix(ext, ".otf") {
		_, err := ParseFont(path)
		return err == nil
	}
	return true
}

var (
	fontCacheMu sync.Mutex
	fontCache   = map[string]*sfnt.Font{}
)

// ParseFont loads and parses a TTF/OTF font file (cached by path).
func ParseFont(path string) (*sfnt.Font, error) {
	fontCacheMu.Lock()
	defer fontCacheMu.Unlock()
	if f, ok := fontCache[path]; ok {
		return f, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := opentype.Parse(raw)
	if err != nil {
		return nil, err
	}
	fontCache[path] = f
	return f, nil
}

// NewFace creates a font.Face at the given size in points (DPI 72, so
// the size equals the pixel em size, matching PIL truetype(size)).
func NewFace(path string, size float64) (font.Face, error) {
	f, err := ParseFont(path)
	if err != nil {
		return nil, err
	}
	if size < 1 {
		size = 1
	}
	return opentype.NewFace(f, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
}
