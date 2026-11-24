package style

import (
	"image"
	"math"

	"image/color"
	"yingku/internal/cover/enhance"
	"yingku/internal/cover/imgx"
)

// RenderCinematic ports style_cinematic.create_style_cinematic:
// full-bleed backdrop, power-curve gradient darkening at the bottom and
// left, big white title bottom-left with tracked English under it.
func RenderCinematic(imagePath string, p Params) (*image.RGBA, error) {
	raw, err := LoadImageFile(imagePath)
	if err != nil {
		return nil, err
	}
	w, h := p.Width, p.Height
	img := imgx.Fit(raw, w, h)
	img = enhance.EnhanceBackdrop(img)
	img = imgx.AdjustColor(img, 1.05)
	img = imgx.AdjustContrast(img, 1.03)

	// Smooth gradient darkening: bottom-dominant + left-assist, power
	// curves avoid hard edges.
	vgrad := imgx.PointMapGray(imgx.LinearGradientV(w, h), func(v int) int {
		return int(235 * math.Pow(float64(v)/255.0, 1.7))
	})
	hgrad := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 255.0
			if w > 1 {
				v = 255.0 * float64(w-1-x) / float64(w-1)
			}
			hgrad.Pix[y*hgrad.Stride+x] = uint8(int(140 * math.Pow(v/255.0, 1.4)))
		}
	}
	mask := imgx.LighterGray(vgrad, hgrad)
	img = imgx.CompositeGray(mask, imgx.NewRGBA(w, h, black), img)

	tk := &TextKit{Img: img}
	zhFont, err := FitFontAdvance(p.ZhFontPath, p.TitleZh, p.FontSizeZh, int(float64(w)*0.84))
	if err != nil {
		return nil, err
	}
	enFont, err := Face(p.EnFontPath, p.FontSizeEn)
	if err != nil {
		enFont, err = Face(p.ZhFontPath, p.FontSizeEn)
		if err != nil {
			return nil, err
		}
	}

	marginX := int(float64(w) * 0.05)
	bottomMargin := int(float64(h) * 0.06)
	gap := int(float64(h) * 0.028)
	enH := 0
	if p.TitleEn != "" {
		enH = InkHeight(enFont, p.TitleEn)
	}
	zhH := InkHeight(zhFont, p.TitleZh)
	enTop := h - bottomMargin - enH
	zhTop := enTop - gap - zhH

	shadowOffset := w / 640
	if shadowOffset < 2 {
		shadowOffset = 2
	}
	tk.Draw(zhFont, float64(marginX+shadowOffset), float64(zhTop+shadowOffset), p.TitleZh, black)
	tk.Draw(zhFont, float64(marginX), float64(zhTop), p.TitleZh, white)
	if p.TitleEn != "" {
		tracking := float64(int(p.FontSizeEn)) * 0.43
		tk.DrawTracked(enFont, float64(marginX+4), float64(enTop), p.TitleEn, color.RGBA{205, 205, 212, 255}, tracking)
	}

	return imgx.RoundedCorners(img, p.CornerRadius()), nil
}
