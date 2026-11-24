package cover

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// TestRenderAllStyles renders every ported style from a synthetic
// backdrop set and checks the output is a 1920x1080 PNG.
func TestRenderAllStyles(t *testing.T) {
	dir := t.TempDir()
	// Three bright, varied "backdrops" (editorial needs 3).
	for i, c := range []color.RGBA{{180, 60, 40, 255}, {40, 90, 160, 255}, {60, 140, 80, 255}} {
		img := image.NewRGBA(image.Rect(0, 0, 1280, 720))
		for y := 0; y < 720; y++ {
			for x := 0; x < 1280; x++ {
				img.Set(x, y, c)
			}
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
			t.Fatal(err)
		}
		name := string(rune('1'+i)) + ".jpg"
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fontDir := t.TempDir()
	zh, en, serif, err := FontPaths(fontDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, styleName := range Styles {
		out, err := Render(styleName, dir, Params("钢铁侠", "MOVIES", zh, en, serif))
		if err != nil {
			t.Fatalf("render %s: %v", styleName, err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("decode %s: %v", styleName, err)
		}
		if cfg.Width != 1920 || cfg.Height != 1080 {
			t.Errorf("style %s size = %dx%d, want 1920x1080", styleName, cfg.Width, cfg.Height)
		}
	}
	if _, err := Render("static_1", dir, Params("x", "MOVIES", zh, en, serif)); err == nil {
		t.Errorf("unported style must error explicitly")
	}
	if RequiredItems("editorial") != 3 || RequiredItems("noir") != 1 {
		t.Errorf("required items = %d/%d", RequiredItems("editorial"), RequiredItems("noir"))
	}
}
