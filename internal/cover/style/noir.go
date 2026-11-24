package style

import (
	"image"
	"image/color"
	"math"
	"math/rand"

	"yingku/internal/cover/enhance"
	"yingku/internal/cover/imgx"
)

// RenderNoir ports style_noir.create_style_noir: full-bleed desaturated
// high-contrast black & white with grain and vignette; white title over a
// white rule bottom-left, tracked English below the rule.
func RenderNoir(imagePath string, p Params) (*image.RGBA, error) {
	raw, err := LoadImageFile(imagePath)
	if err != nil {
		return nil, err
	}
	w, h := p.Width, p.Height
	img := imgx.Fit(raw, w, h)
	img = enhance.EnhanceBackdrop(img)

	// Desaturate + high contrast.
	img = imgx.ToRGBA(imgx.Gray(img))
	img = imgx.AdjustContrast(img, 1.18)
	img = imgx.AdjustBrightness(img, 0.99)

	// Grain: screen-blend gaussian noise at 10%.
	rng := rand.New(rand.NewSource(20240521))
	noise := imgx.NoiseGray(w, h, 22, rng)
	grain := imgx.ToRGBA(noise)
	img = imgx.Blend(img, imgx.Screen(img, grain), 0.10)

	// Vignette.
	marginX, marginY := int(float64(w)*0.18), int(float64(h)*0.18)
	vigMask := imgx.EllipseMaskGray(w, h, image.Rect(-marginX, -marginY, w+marginX, h+marginY))
	minSide := w
	if h < minSide {
		minSide = h
	}
	vigMask = imgx.BlurGray(vigMask, float64(minSide/6))
	dark := imgx.AdjustBrightness(img, 0.45)
	img = imgx.CompositeGray(vigMask, img, dark)

	// Bottom gradient so the white type stays readable.
	grad := imgx.LinearGradientV(w, h)
	vmask := imgx.PointMapGray(grad, func(v int) int {
		return int(165 * math.Pow(float64(v)/255.0, 1.7))
	})
	img = imgx.CompositeGray(vmask, imgx.NewRGBA(w, h, black), img)

	tk := &TextKit{Img: img}
	x0 := int(float64(w) * 0.052)
	bottomMargin := int(float64(h) * 0.055)

	enFont, err := Face(p.EnFontPath, p.FontSizeEn*0.8)
	if err != nil {
		enFont, err = Face(p.ZhFontPath, p.FontSizeEn*0.8)
		if err != nil {
			return nil, err
		}
	}
	enHeight := InkHeight(enFont, p.TitleEn)
	enY := h - bottomMargin - enHeight

	zhFont, err := FitFontInk(p.ZhFontPath, p.TitleZh, p.FontSizeZh, int(float64(w)*0.62))
	if err != nil {
		return nil, err
	}
	zhHeight := InkHeight(zhFont, p.TitleZh)
	ruleGap := int(float64(h) * 0.022)
	ruleH := h / 240
	if ruleH < 3 {
		ruleH = 3
	}
	ruleY := enY - ruleGap - ruleH
	zhY := ruleY - ruleGap - zhHeight

	zhWidth := InkWidth(zhFont, p.TitleZh)
	shadow := w / 640
	if shadow < 2 {
		shadow = 2
	}
	tk.DrawInk(zhFont, float64(x0+shadow), float64(zhY+shadow), p.TitleZh, black)
	tk.DrawInk(zhFont, float64(x0), float64(zhY), p.TitleZh, white)
	FillRectIncl(img, x0, ruleY, x0+zhWidth, ruleY+ruleH, white)
	tk.DrawTracked(enFont, float64(x0), float64(enY), p.TitleEn, color.RGBA{235, 235, 235, 255}, p.FontSizeEn*0.55)

	return imgx.RoundedCorners(img, p.CornerRadius()), nil
}
