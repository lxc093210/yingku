// Package cover renders library covers in-process: a faithful port of
// the standalone mediacover-go generator's rendering stack (imgx /
// enhance / style / fonts), driven by yingku's own store instead of
// the Emby HTTP API. The two production fonts are binary assets that
// cannot travel with the source snapshot, so they are loaded from
// disk (see FontPaths) and cached into the data directory.
package cover

import (
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"

	"yingku/internal/cover/imgx"
	"yingku/internal/cover/style"
)

// Styles lists the ported cover styles, in picker order.
var Styles = []string{"editorial", "cinematic", "retro", "editorial_big", "noir"}

// IsStyle reports whether name is a renderable style.
func IsStyle(name string) bool {
	for _, s := range Styles {
		if s == name {
			return true
		}
	}
	return false
}

// RequiredItems is the number of backdrop picks a style consumes.
func RequiredItems(styleName string) int { return style.RequiredItems(styleName) }

// FontPaths copies the production fonts into dir (once) and returns
// the zh / en / serif file paths. Production parity with the Python
// and Go generators: zh = chaohei, en = yasong (the EmblemaOne preset
// is WOFF2, which Go cannot parse; the Python production run fell
// back to yasong for English as well), serif = yasong.
//
// The .ttf files are binary and are NOT part of the source snapshot;
// they are provisioned on the host. Source directories are tried in
// order: $YINGKU_FONT_DIR, <data>/fonts (dir is <data>/cache/
// cover-fonts), then repo-relative assets/fonts for local dev/tests.
func FontPaths(dir string) (zh, en, serif string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	sources := []string{}
	if v := os.Getenv("YINGKU_FONT_DIR"); v != "" {
		sources = append(sources, v)
	}
	sources = append(sources,
		filepath.Join(dir, "..", "..", "fonts"),
		"assets/fonts",
		filepath.Join("internal", "cover", "assets", "fonts"),
	)
	find := func(name string) (string, error) {
		dst := filepath.Join(dir, name)
		for _, srcDir := range sources {
			src := filepath.Join(srcDir, name)
			if src == dst {
				if _, err := os.Stat(src); err == nil {
					return dst, nil
				}
				continue
			}
			sfi, err := os.Stat(src)
			if err != nil || sfi.Size() == 0 {
				continue
			}
			if dfi, err := os.Stat(dst); err == nil && dfi.Size() == sfi.Size() {
				return dst, nil
			}
			in, err := os.Open(src)
			if err != nil {
				continue
			}
			tmp := dst + ".tmp"
			out, err := os.Create(tmp)
			if err != nil {
				in.Close()
				return "", err
			}
			_, copyErr := io.Copy(out, in)
			closeErr := out.Close()
			in.Close()
			if copyErr != nil || closeErr != nil {
				_ = os.Remove(tmp)
				return "", fmt.Errorf("copy font %s: %v %v", name, copyErr, closeErr)
			}
			if err := os.Rename(tmp, dst); err != nil {
				return "", err
			}
			return dst, nil
		}
		return "", fmt.Errorf("font %s not found in any source dir %v (provision it under <data>/fonts)", name, sources)
	}
	if zh, err = find("chaohei.ttf"); err != nil {
		return "", "", "", err
	}
	if en, err = find("yasong.ttf"); err != nil {
		return "", "", "", err
	}
	return zh, en, en, nil
}

// Params builds the style parameters for a 1920x1080 cover with the
// production font sizes (zh 170 / en 75 at 1080p, title scale 1.0).
func Params(zhTitle, enTitle, zhFont, enFont, serifFont string) style.Params {
	return style.Params{
		TitleZh:       zhTitle,
		TitleEn:       enTitle,
		ZhFontPath:    zhFont,
		EnFontPath:    enFont,
		SerifFontPath: serifFont,
		FontSizeZh:    170,
		FontSizeEn:    75,
		Width:         1920,
		Height:        1080,
	}
}

// Render draws one cover of the given style from the numbered images
// (1.jpg..N.jpg) in imageDir and returns the PNG bytes.
func Render(styleName, imageDir string, params style.Params) ([]byte, error) {
	var canvas *image.RGBA
	var err error
	switch styleName {
	case "cinematic":
		canvas, err = style.RenderCinematic(filepath.Join(imageDir, "1.jpg"), params)
	case "noir":
		canvas, err = style.RenderNoir(filepath.Join(imageDir, "1.jpg"), params)
	case "retro":
		canvas, err = style.RenderRetro(filepath.Join(imageDir, "1.jpg"), params)
	case "editorial":
		canvas, err = style.RenderEditorial(imageDir, params)
	case "editorial_big":
		canvas, err = style.RenderEditorialBig(imageDir, params)
	default:
		return nil, fmt.Errorf("unknown cover style %q", styleName)
	}
	if err != nil {
		return nil, err
	}
	return imgx.EncodePNG(canvas)
}
