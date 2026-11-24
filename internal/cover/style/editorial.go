package style

import (
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"

	"yingku/internal/cover/enhance"
	"yingku/internal/cover/imgx"
)

// RenderEditorial ports style_editorial.create_style_editorial:
// cream paper, serif title on the left, three framed stills stacked on
// the right with soft shadows.
func RenderEditorial(imageDir string, p Params) (*image.RGBA, error) {
	w, h := p.Width, p.Height
	img := imgx.NewRGBA(w, h, paperCol)

	// Right-hand still column.
	stillW := int(float64(w) * 0.292)
	stillH := int(float64(stillW) * 9 / 16)
	gap := int(float64(h) * 0.028)
	pad := w / 240
	if pad < 6 {
		pad = 6
	}
	x1 := w - int(float64(w)*0.057) - stillW

	type still struct {
		img *image.RGBA
	}
	var stills []still
	for _, index := range []int{1, 2, 3} {
		candidate := filepath.Join(imageDir, fmt.Sprintf("%d.jpg", index))
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		raw, err := LoadImageFile(candidate)
		if err != nil {
			continue
		}
		fitted := imgx.Fit(raw, stillW, stillH)
		stills = append(stills, still{img: enhance.EnhanceBackdrop(fitted)})
	}
	totalH := len(stills)*stillH + max(0, len(stills)-1)*gap
	y := (h - totalH) / 2
	for _, st := range stills {
		shadow := image.NewNRGBA(image.Rect(0, 0, w, h))
		FillRectInclNRGBA(shadow, x1-pad, y+4, x1+stillW+pad, y+stillH+2*pad+4, color.NRGBA{0, 0, 0, 70})
		shadow = imgx.GaussianBlurNRGBA(shadow, 10)
		img = imgx.AlphaOver(img, shadow)
		FillRectIncl(img, x1-pad, y-pad, x1+stillW+pad, y+stillH+pad, white)
		imgx.Paste(img, st.img, x1, y)
		y += stillH + gap
	}

	// Left title block.
	tk := &TextKit{Img: img}
	x0 := int(float64(w) * 0.068)
	maxTitleWidth := x1 - x0 - int(float64(w)*0.04)
	zhFont, err := FitFontAdvance(p.SerifFontPath, p.TitleZh, p.FontSizeZh*1.2, maxTitleWidth)
	if err != nil {
		return nil, err
	}
	enFont, err := Face(p.SerifFontPath, p.FontSizeEn*0.78)
	if err != nil {
		return nil, err
	}
	zhTop := int(float64(h) * 0.36)
	tk.Draw(zhFont, float64(x0), float64(zhTop), p.TitleZh, inkEditor)
	zhH := InkHeight(zhFont, p.TitleZh)
	enTop := zhTop + zhH + int(float64(h)*0.045)
	if p.TitleEn != "" {
		tracking := float64(int(p.FontSizeEn)) * 0.36
		tk.DrawTracked(enFont, float64(x0+4), float64(enTop), p.TitleEn, inkEditor, tracking)
	}
	enText := p.TitleEn
	if enText == "" {
		enText = "MOVIES"
	}
	enH := InkHeight(enFont, enText)
	lineY := enTop + enH + int(float64(h)*0.05)
	lineWidth := h / 360
	if lineWidth < 2 {
		lineWidth = 2
	}
	HLine(img, x0+4, x0+int(float64(w)*0.18), lineY, lineWidth, inkEditor)

	return imgx.RoundedCorners(img, p.CornerRadius()), nil
}
