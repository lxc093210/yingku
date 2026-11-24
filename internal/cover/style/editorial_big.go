package style

import (
	"image"
	"image/color"
	"path/filepath"

	"yingku/internal/cover/enhance"
	"yingku/internal/cover/imgx"
)

// RenderEditorialBig ports style_editorial_big.create_style_editorial_big:
// cream paper + serif title on the left, one hero still plus two small
// stills on the right.
func RenderEditorialBig(imageDir string, p Params) (*image.RGBA, error) {
	w, h := p.Width, p.Height
	img := imgx.NewRGBA(w, h, paperCol)
	tk := &TextKit{Img: img}

	// Left title block.
	x0 := int(float64(w) * 0.052)
	rx0 := int(float64(w) * 0.505)
	avail := rx0 - x0 - int(float64(w)*0.045)
	zhFont, err := FitFontInk(p.SerifFontPath, p.TitleZh, p.FontSizeZh*1.2, avail)
	if err != nil {
		return nil, err
	}
	zhHeight := InkHeight(zhFont, p.TitleZh)
	zhTop := int(float64(h) * 0.335)
	tk.DrawInk(zhFont, float64(x0), float64(zhTop), p.TitleZh, inkBig)

	enSize := int(p.FontSizeEn * 0.78)
	if enSize < 18 {
		enSize = 18
	}
	enFont, err := Face(p.SerifFontPath, float64(enSize))
	if err != nil {
		return nil, err
	}
	enY := zhTop + zhHeight + int(float64(h)*0.045)
	tk.DrawTracked(enFont, float64(x0), float64(enY), p.TitleEn, inkBig, p.FontSizeEn*0.36)
	ruleY := enY + enSize + int(float64(h)*0.038)
	lineWidth := h / 360
	if lineWidth < 2 {
		lineWidth = 2
	}
	HLine(img, x0, x0+int(float64(w)*0.18), ruleY, lineWidth, inkBig)

	// Right: 1 hero + 2 small.
	rw := w - int(float64(w)*0.052) - rx0
	heroH := rw * 9 / 16
	gap := int(float64(h) * 0.024)
	sw := (rw - gap) / 2
	sh := sw * 9 / 16
	top := (h - (heroH + gap + sh)) / 2
	pad := w / 240
	if pad < 6 {
		pad = 6
	}

	place := func(name string, x, y, bw, bh int) {
		candidate := filepath.Join(imageDir, name)
		raw, err := LoadImageFile(candidate)
		if err != nil {
			return
		}
		shadow := image.NewNRGBA(image.Rect(0, 0, bw+pad*2, bh+pad*2))
		FillRectInclNRGBA(shadow, pad, pad, pad+bw, pad+bh, color.NRGBA{30, 25, 18, 110})
		shadow = imgx.GaussianBlurNRGBA(shadow, 10)
		imgx.PasteWithMask(img, shadow, x-pad, y-pad+6)
		fitted := enhance.EnhanceBackdrop(imgx.Fit(raw, bw, bh))
		frame := imgx.NewRGBA(bw+pad*2, bh+pad*2, white)
		imgx.Paste(frame, fitted, pad, pad)
		imgx.Paste(img, frame, x-pad, y-pad)
	}
	place("1.jpg", rx0, top, rw, heroH)
	place("2.jpg", rx0, top+heroH+gap, sw, sh)
	place("3.jpg", rx0+sw+gap, top+heroH+gap, sw, sh)

	return imgx.RoundedCorners(img, p.CornerRadius()), nil
}
