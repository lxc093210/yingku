package style

import (
	"image"
	"image/color"
	"math/rand"

	"yingku/internal/cover/enhance"
	"yingku/internal/cover/imgx"
)

var (
	duotoneBlack = color.RGBA{0x2e, 0x1c, 0x11, 255}
	duotoneMid   = color.RGBA{0xc0, 0x8b, 0x58, 255}
	duotoneWhite = color.RGBA{0xf7, 0xea, 0xd0, 255}
	darkerBlack  = color.RGBA{0x1d, 0x11, 0x0a, 255}
	darkerMid    = color.RGBA{0x8a, 0x5a, 0x34, 255}
	darkerWhite  = color.RGBA{0xe8, 0xd3, 0xac, 255}
	titleRed     = color.RGBA{164, 34, 26, 255}
	titleStroke  = color.RGBA{247, 234, 205, 255}
	enCream      = color.RGBA{240, 226, 196, 255}
	frameRed     = color.RGBA{150, 44, 30, 255}
	stripColor   = color.RGBA{16, 13, 11, 255}
	holeColor    = color.RGBA{233, 220, 192, 255}
)

// RenderRetro ports style_retro.create_style_retro: duotone-aged backdrop
// with grain and vignette, double red inner frame, film sprocket strips
// top and bottom, deep-red stroked title (2x2 grid for 4-rune titles).
func RenderRetro(imagePath string, p Params) (*image.RGBA, error) {
	raw, err := LoadImageFile(imagePath)
	if err != nil {
		return nil, err
	}
	w, h := p.Width, p.Height
	base := enhance.EnhanceBackdrop(imgx.Fit(raw, w, h))
	base = imgx.AdjustContrast(base, 1.06)
	gray := imgx.Gray(base)
	img := imgx.Colorize(gray, duotoneBlack, duotoneMid, duotoneWhite)

	// Grain.
	rng := rand.New(rand.NewSource(19691231))
	noise := imgx.NoiseGray(w, h, 30, rng)
	noiseColor := imgx.Colorize(noise, duotoneBlack, duotoneMid, duotoneWhite)
	img = imgx.Blend(img, noiseColor, 0.10)

	// Vignette (ported literally: composite with the inverted mask).
	vig := imgx.EllipseMaskGray(w, h, image.Rect(
		-int(float64(w)*0.25), -int(float64(h)*0.35),
		int(float64(w)*1.25), int(float64(h)*1.35)))
	minSide := w
	if h < minSide {
		minSide = h
	}
	vig = imgx.BlurGray(vig, float64(minSide)*0.17)
	darker := imgx.Colorize(gray, darkerBlack, darkerMid, darkerWhite)
	img = imgx.CompositeGray(imgx.InvertGray(vig), img, darker)

	tk := &TextKit{Img: img}
	stripH := int(float64(h) * 0.089)

	// Inner double red frame.
	frameX := int(float64(w) * 0.021)
	frameTop := stripH + int(float64(h)*0.03)
	frameBottom := h - stripH - int(float64(h)*0.03)
	frameW := w / 480
	if frameW < 2 {
		frameW = 2
	}
	StrokeRectIncl(img, frameX, frameTop, w-frameX, frameBottom, frameW, frameRed)
	inset := int(float64(w) * 0.0073)
	frameW2 := w / 960
	if frameW2 < 1 {
		frameW2 = 1
	}
	StrokeRectIncl(img, frameX+inset, frameTop+inset, w-frameX-inset, frameBottom-inset, frameW2, frameRed)

	// Title.
	titleX := int(float64(w) * 0.0625)
	titleY := int(float64(h) * 0.20)
	runes := []rune(p.TitleZh)
	var blockBottom int
	if len(runes) == 4 {
		size := int(p.FontSizeZh * 1.5)
		for size > 40 {
			face, err := Face(p.ZhFontPath, float64(size))
			if err != nil {
				return nil, err
			}
			if AdvanceWidth(face, string(runes[0]))*2.08 <= float64(w)*0.36 {
				break
			}
			size = int(float64(size) * 0.94)
		}
		zhFont, err := Face(p.ZhFontPath, float64(size))
		if err != nil {
			return nil, err
		}
		cell := int(float64(size) * 1.04)
		stroke := size / 50
		if stroke < 2 {
			stroke = 2
		}
		for row := 0; row < 2; row++ {
			for col := 0; col < 2; col++ {
				ch := string(runes[row*2+col])
				tk.DrawStroke(zhFont, float64(titleX+col*cell), float64(titleY+row*cell), ch, titleRed, titleStroke, stroke)
			}
		}
		blockBottom = titleY + 2*cell
	} else {
		zhFont, err := FitFontAdvance(p.ZhFontPath, p.TitleZh, p.FontSizeZh*1.5, int(float64(w)*0.6))
		if err != nil {
			return nil, err
		}
		stroke := int(p.FontSizeZh*1.5) / 50
		if stroke < 2 {
			stroke = 2
		}
		tk.DrawStroke(zhFont, float64(titleX), float64(titleY), p.TitleZh, titleRed, titleStroke, stroke)
		blockBottom = titleY + InkHeight(zhFont, p.TitleZh)
	}

	if p.TitleEn != "" {
		enFont, err := Face(p.EnFontPath, p.FontSizeEn*0.86)
		if err != nil {
			enFont, err = Face(p.ZhFontPath, p.FontSizeEn*0.86)
			if err != nil {
				return nil, err
			}
		}
		tracking := float64(int(p.FontSizeEn)) * 0.48
		tk.DrawTracked(enFont, float64(titleX+6), float64(blockBottom+int(float64(h)*0.033)), p.TitleEn, enCream, tracking)
	}

	// Film sprocket strips.
	FillRectIncl(img, 0, 0, w, stripH, stripColor)
	FillRectIncl(img, 0, h-stripH, w, h, stripColor)
	holeW := int(float64(w) * 0.0333)
	holeH := int(float64(stripH) * 0.54)
	holeGap := int(float64(w) * 0.023)
	count := (w + holeGap) / (holeW + holeGap)
	x0 := (w - (count*holeW + (count-1)*holeGap)) / 2
	holeRadius := holeH / 5
	if holeRadius < 4 {
		holeRadius = 4
	}
	for i := 0; i < count; i++ {
		x := x0 + i*(holeW+holeGap)
		for _, y0 := range []int{(stripH - holeH) / 2, h - stripH + (stripH-holeH)/2} {
			FillRoundedRect(img, x, y0, x+holeW, y0+holeH, holeRadius, holeColor)
		}
	}

	return imgx.RoundedCorners(img, p.CornerRadius()), nil
}
