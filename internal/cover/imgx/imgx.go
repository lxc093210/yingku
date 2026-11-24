// Package imgx provides the PIL operations the styles rely on, implemented
// on the standard library plus golang.org/x/image/draw.
//
// Approximations vs PIL (visual parity, not bit parity):
//   - ImageOps.fit uses draw.CatmullRom instead of PIL LANCZOS.
//   - GaussianBlur is the classic three-box-blur approximation of a gaussian
//     (Pillow itself implements GaussianBlur with box passes internally).
//   - Resize for statistics thumbnails uses ApproxBiLinear.
package imgx

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoding
)

// DecodeBytes decodes JPEG/PNG/WebP/GIF bytes like PIL Image.open.
func DecodeBytes(content []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(content))
	return img, err
}

// SniffContentType mirrors generator.decode_image's base64-prefix sniffing,
// done here on the raw bytes: PNG default, GIF, WebP, JPEG.
func SniffContentType(b []byte) (contentType, extension string) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg", "jpg"
	case len(b) >= 4 && string(b[:4]) == "GIF8":
		return "image/gif", "gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp", "webp"
	default:
		return "image/png", "png"
	}
}

// EncodePNG encodes like PIL save(format="PNG", optimize=True)
// (stdlib BestCompression is the closest equivalent).
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ToRGBA converts any image to an opaque *image.RGBA (PIL convert("RGB")).
func ToRGBA(src image.Image) *image.RGBA {
	if rgba, ok := src.(*image.RGBA); ok {
		out := image.NewRGBA(rgba.Bounds())
		draw.Draw(out, out.Bounds(), rgba, rgba.Bounds().Min, draw.Src)
		return out
	}
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	// Force opaque alpha like PIL's RGB mode.
	for i := 3; i < len(out.Pix); i += 4 {
		out.Pix[i] = 0xFF
	}
	return out
}

// CloneRGBA deep-copies an RGBA image.
func CloneRGBA(src *image.RGBA) *image.RGBA {
	out := image.NewRGBA(src.Bounds())
	copy(out.Pix, src.Pix)
	out.Stride = src.Stride
	return out
}

// NewRGBA creates a w*h RGBA filled with c.
func NewRGBA(w, h int, c color.RGBA) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	return out
}

// Resize scales src to exactly w*h with the given interpolator.
func Resize(src image.Image, w, h int, interp xdraw.Interpolator) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	interp.Scale(out, out.Bounds(), src, src.Bounds(), draw.Src, nil)
	return out
}

// Fit mirrors PIL ImageOps.fit: scale to cover (w,h) then center-crop.
func Fit(src image.Image, w, h int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == w && sh == h {
		return ToRGBA(src)
	}
	scale := math.Max(float64(w)/float64(sw), float64(h)/float64(sh))
	nw := int(math.Round(float64(sw) * scale))
	nh := int(math.Round(float64(sh) * scale))
	if nw < w {
		nw = w
	}
	if nh < h {
		nh = h
	}
	scaled := Resize(src, nw, nh, xdraw.CatmullRom)
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	ox := (nw - w) / 2
	oy := (nh - h) / 2
	draw.Draw(out, out.Bounds(), scaled, image.Pt(ox, oy), draw.Src)
	return out
}

// Gray converts to 8-bit grayscale (PIL convert("L"), ITU-R 601-2).
func Gray(src image.Image) *image.Gray {
	b := src.Bounds()
	out := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

// MeanStd returns the mean and standard deviation of a gray image
// (PIL ImageStat.Stat mean/stddev).
func MeanStd(g *image.Gray) (mean, std float64) {
	var sum float64
	for _, v := range g.Pix {
		sum += float64(v)
	}
	mean = sum / float64(len(g.Pix))
	var sq float64
	for _, v := range g.Pix {
		d := float64(v) - mean
		sq += d * d
	}
	return mean, math.Sqrt(sq / float64(len(g.Pix)))
}

func clamp(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v + 0.5)
}

func eachPixel(img *image.RGBA, fn func(r, g, b uint8) (uint8, uint8, uint8)) {
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2] = fn(img.Pix[i], img.Pix[i+1], img.Pix[i+2])
	}
}

// AdjustBrightness mirrors PIL ImageEnhance.Brightness.
func AdjustBrightness(img *image.RGBA, factor float64) *image.RGBA {
	out := CloneRGBA(img)
	eachPixel(out, func(r, g, b uint8) (uint8, uint8, uint8) {
		return clamp(float64(r) * factor), clamp(float64(g) * factor), clamp(float64(b) * factor)
	})
	return out
}

// AdjustContrast mirrors PIL ImageEnhance.Contrast (mean of the grayscale
// image, rounded, is the pivot).
func AdjustContrast(img *image.RGBA, factor float64) *image.RGBA {
	mean, _ := MeanStd(Gray(img))
	pivot := math.Round(mean)
	out := CloneRGBA(img)
	eachPixel(out, func(r, g, b uint8) (uint8, uint8, uint8) {
		return clamp((float64(r)-pivot)*factor + pivot),
			clamp((float64(g)-pivot)*factor + pivot),
			clamp((float64(b)-pivot)*factor + pivot)
	})
	return out
}

// AdjustColor mirrors PIL ImageEnhance.Color (0 = grayscale, 1 = original).
func AdjustColor(img *image.RGBA, factor float64) *image.RGBA {
	gray := Gray(img)
	out := CloneRGBA(img)
	for i, j := 0, 0; i < len(out.Pix); i, j = i+4, j+1 {
		g := float64(gray.Pix[j])
		out.Pix[i] = clamp(g + (float64(out.Pix[i])-g)*factor)
		out.Pix[i+1] = clamp(g + (float64(out.Pix[i+1])-g)*factor)
		out.Pix[i+2] = clamp(g + (float64(out.Pix[i+2])-g)*factor)
	}
	return out
}

// boxBlurPass applies one separable box blur of the given radius to every
// channel (including alpha) of an RGBA image, with edge replication.
func boxBlurPass(img *image.RGBA, radius int) *image.RGBA {
	if radius <= 0 {
		return CloneRGBA(img)
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	tmp := image.NewRGBA(img.Bounds())
	out := image.NewRGBA(img.Bounds())
	window := 2*radius + 1
	// Horizontal.
	for y := 0; y < h; y++ {
		rowOff := y * img.Stride
		for ch := 0; ch < 4; ch++ {
			var acc int
			for k := -radius; k <= radius; k++ {
				x := k
				if x < 0 {
					x = 0
				}
				acc += int(img.Pix[rowOff+x*4+ch])
			}
			for x := 0; x < w; x++ {
				tmp.Pix[rowOff+x*4+ch] = uint8(acc / window)
				xOut := x - radius
				if xOut < 0 {
					xOut = 0
				}
				xIn := x + radius + 1
				if xIn >= w {
					xIn = w - 1
				}
				acc += int(img.Pix[rowOff+xIn*4+ch]) - int(img.Pix[rowOff+xOut*4+ch])
			}
		}
	}
	// Vertical.
	for x := 0; x < w; x++ {
		for ch := 0; ch < 4; ch++ {
			var acc int
			for k := -radius; k <= radius; k++ {
				y := k
				if y < 0 {
					y = 0
				}
				acc += int(tmp.Pix[y*tmp.Stride+x*4+ch])
			}
			for y := 0; y < h; y++ {
				out.Pix[y*out.Stride+x*4+ch] = uint8(acc / window)
				yOut := y - radius
				if yOut < 0 {
					yOut = 0
				}
				yIn := y + radius + 1
				if yIn >= h {
					yIn = h - 1
				}
				acc += int(tmp.Pix[yIn*tmp.Stride+x*4+ch]) - int(tmp.Pix[yOut*tmp.Stride+x*4+ch])
			}
		}
	}
	return out
}

// GaussianBlur approximates PIL ImageFilter.GaussianBlur(radius) with three
// box blurs whose sizes are derived from the sigma (W3C filter-effects
// algorithm), operating per channel on a straight (non-premultiplied) RGBA.
func GaussianBlur(img *image.RGBA, radius float64) *image.RGBA {
	if radius <= 0 {
		return CloneRGBA(img)
	}
	// The images blurred through this path (backdrops, unsharp masks) are
	// opaque, so premultiplied RGBA channels blur exactly like PIL's RGB
	// mode. Shadow layers with real alpha use GaussianBlurNRGBA instead.
	boxes := boxSizesForGaussian(radius, 3)
	out := CloneRGBA(img)
	for _, size := range boxes {
		out = boxBlurPass(out, (size-1)/2)
	}
	return out
}

// ToNRGBA converts to straight-alpha NRGBA.
func ToNRGBA(src image.Image) *image.NRGBA {
	if n, ok := src.(*image.NRGBA); ok {
		out := image.NewNRGBA(n.Bounds())
		copy(out.Pix, n.Pix)
		return out
	}
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), src, b.Min, draw.Src)
	return out
}

func boxBlurNRGBA(img *image.NRGBA, radius int) *image.NRGBA {
	if radius <= 0 {
		out := image.NewNRGBA(img.Bounds())
		copy(out.Pix, img.Pix)
		return out
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	tmp := image.NewNRGBA(img.Bounds())
	out := image.NewNRGBA(img.Bounds())
	window := 2*radius + 1
	for y := 0; y < h; y++ {
		rowOff := y * img.Stride
		for ch := 0; ch < 4; ch++ {
			var acc int
			for k := -radius; k <= radius; k++ {
				x := k
				if x < 0 {
					x = 0
				}
				acc += int(img.Pix[rowOff+x*4+ch])
			}
			for x := 0; x < w; x++ {
				tmp.Pix[rowOff+x*4+ch] = uint8(acc / window)
				xOut := x - radius
				if xOut < 0 {
					xOut = 0
				}
				xIn := x + radius + 1
				if xIn >= w {
					xIn = w - 1
				}
				acc += int(img.Pix[rowOff+xIn*4+ch]) - int(img.Pix[rowOff+xOut*4+ch])
			}
		}
	}
	for x := 0; x < w; x++ {
		for ch := 0; ch < 4; ch++ {
			var acc int
			for k := -radius; k <= radius; k++ {
				y := k
				if y < 0 {
					y = 0
				}
				acc += int(tmp.Pix[y*tmp.Stride+x*4+ch])
			}
			for y := 0; y < h; y++ {
				out.Pix[y*out.Stride+x*4+ch] = uint8(acc / window)
				yOut := y - radius
				if yOut < 0 {
					yOut = 0
				}
				yIn := y + radius + 1
				if yIn >= h {
					yIn = h - 1
				}
				acc += int(tmp.Pix[yIn*tmp.Stride+x*4+ch]) - int(tmp.Pix[yOut*tmp.Stride+x*4+ch])
			}
		}
	}
	return out
}

// gaussianBlurNRGBA blurs a straight-alpha image.
func gaussianBlurNRGBA(img *image.NRGBA, sigma float64) *image.NRGBA {
	boxes := boxSizesForGaussian(sigma, 3)
	out := img
	for _, size := range boxes {
		out = boxBlurNRGBA(out, (size-1)/2)
	}
	return out
}

// GaussianBlurNRGBA is the exported NRGBA variant (used for shadow layers
// and masks, where alpha must blur independently like PIL).
func GaussianBlurNRGBA(img *image.NRGBA, radius float64) *image.NRGBA {
	if radius <= 0 {
		out := image.NewNRGBA(img.Bounds())
		copy(out.Pix, img.Pix)
		return out
	}
	return gaussianBlurNRGBA(img, radius)
}

// BlurGray blurs a grayscale image (vignette masks).
func BlurGray(img *image.Gray, radius float64) *image.Gray {
	n := image.NewNRGBA(img.Bounds())
	for i, v := range img.Pix {
		n.Pix[i*4] = v
		n.Pix[i*4+1] = v
		n.Pix[i*4+2] = v
		n.Pix[i*4+3] = 0xFF
	}
	blurred := GaussianBlurNRGBA(n, radius)
	out := image.NewGray(img.Bounds())
	for i := range out.Pix {
		out.Pix[i] = blurred.Pix[i*4]
	}
	return out
}

func boxSizesForGaussian(sigma float64, n int) []int {
	wIdeal := math.Sqrt((12 * sigma * sigma / float64(n)) + 1)
	wl := int(math.Floor(wIdeal))
	if wl%2 == 0 {
		wl--
	}
	wu := wl + 2
	mIdeal := (12*sigma*sigma - float64(n*wl*wl) - float64(4*n*wl) - float64(3*n)) / float64(-4*wl-4)
	m := int(math.Round(mIdeal))
	sizes := make([]int, n)
	for i := 0; i < n; i++ {
		if i < m {
			sizes[i] = wl
		} else {
			sizes[i] = wu
		}
	}
	return sizes
}

// UnsharpMask mirrors PIL ImageFilter.UnsharpMask(radius, percent, threshold).
func UnsharpMask(img *image.RGBA, radius float64, percent int, threshold int) *image.RGBA {
	blurred := GaussianBlur(img, radius)
	out := CloneRGBA(img)
	factor := float64(percent) / 100.0
	for i := 0; i < len(out.Pix); i += 4 {
		for ch := 0; ch < 3; ch++ {
			diff := int(out.Pix[i+ch]) - int(blurred.Pix[i+ch])
			if diff > threshold || diff < -threshold {
				out.Pix[i+ch] = clamp(float64(out.Pix[i+ch]) + float64(diff)*factor)
			}
		}
	}
	return out
}

// LinearGradientV mirrors PIL Image.linear_gradient("L") resized to w*h:
// value 0 at the top row increasing to 255 at the bottom row.
func LinearGradientV(w, h int) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		var v uint8
		if h > 1 {
			v = uint8(int(math.Round(float64(y) * 255.0 / float64(h-1))))
		}
		for x := 0; x < w; x++ {
			out.SetGray(x, y, color.Gray{Y: v})
		}
	}
	return out
}

// PointMapGray applies fn to every pixel of a gray image (PIL Image.point).
func PointMapGray(img *image.Gray, fn func(int) int) *image.Gray {
	out := image.NewGray(img.Bounds())
	for i, v := range img.Pix {
		out.Pix[i] = clamp(float64(fn(int(v))))
	}
	return out
}

// LighterGray mirrors PIL ImageChops.lighter (per-pixel max).
func LighterGray(a, b *image.Gray) *image.Gray {
	out := image.NewGray(a.Bounds())
	for i := range out.Pix {
		if a.Pix[i] > b.Pix[i] {
			out.Pix[i] = a.Pix[i]
		} else {
			out.Pix[i] = b.Pix[i]
		}
	}
	return out
}

// InvertGray returns 255-v per pixel.
func InvertGray(img *image.Gray) *image.Gray {
	out := image.NewGray(img.Bounds())
	for i, v := range img.Pix {
		out.Pix[i] = 255 - v
	}
	return out
}

// CompositeGray mirrors PIL Image.composite(image1, image2, mask):
// result = image1 where mask is 255, image2 where mask is 0.
func CompositeGray(mask *image.Gray, image1, image2 *image.RGBA) *image.RGBA {
	out := image.NewRGBA(image1.Bounds())
	for i, j := 0, 0; i < len(out.Pix); i, j = i+4, j+1 {
		m := int(mask.Pix[j])
		for ch := 0; ch < 3; ch++ {
			out.Pix[i+ch] = uint8((int(image1.Pix[i+ch])*m + int(image2.Pix[i+ch])*(255-m) + 127) / 255)
		}
		out.Pix[i+3] = 0xFF
	}
	return out
}

// Blend mirrors PIL Image.blend(im1, im2, alpha).
func Blend(a, b *image.RGBA, alpha float64) *image.RGBA {
	out := image.NewRGBA(a.Bounds())
	for i := 0; i < len(out.Pix); i += 4 {
		for ch := 0; ch < 3; ch++ {
			out.Pix[i+ch] = clamp(float64(a.Pix[i+ch])*(1-alpha) + float64(b.Pix[i+ch])*alpha)
		}
		out.Pix[i+3] = 0xFF
	}
	return out
}

// Screen mirrors PIL ImageChops.screen.
func Screen(a, b *image.RGBA) *image.RGBA {
	out := image.NewRGBA(a.Bounds())
	for i := 0; i < len(out.Pix); i += 4 {
		for ch := 0; ch < 3; ch++ {
			av, bv := int(a.Pix[i+ch]), int(b.Pix[i+ch])
			out.Pix[i+ch] = uint8(255 - ((255-av)*(255-bv))/255)
		}
		out.Pix[i+3] = 0xFF
	}
	return out
}

// AlphaOver composites src (straight alpha) over an opaque RGBA canvas,
// like PIL Image.alpha_composite after converting both to RGBA.
func AlphaOver(dst *image.RGBA, src *image.NRGBA) *image.RGBA {
	out := CloneRGBA(dst)
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()
	w, h := sw, sh
	if dw < w {
		w = dw
	}
	if dh < h {
		h = dh
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			si := y*src.Stride + x*4
			a := int(src.Pix[si+3])
			if a == 0 {
				continue
			}
			di := y*out.Stride + x*4
			if a == 255 {
				out.Pix[di] = src.Pix[si]
				out.Pix[di+1] = src.Pix[si+1]
				out.Pix[di+2] = src.Pix[si+2]
				continue
			}
			for ch := 0; ch < 3; ch++ {
				out.Pix[di+ch] = uint8((int(src.Pix[si+ch])*a + int(out.Pix[di+ch])*(255-a) + 127) / 255)
			}
		}
	}
	return out
}

// Paste draws src opaquely onto dst at (x, y) (PIL Image.paste without mask).
func Paste(dst, src *image.RGBA, x, y int) {
	draw.Draw(dst, image.Rect(x, y, x+src.Bounds().Dx(), y+src.Bounds().Dy()), src, image.Point{}, draw.Src)
}

// PasteWithMask mirrors PIL Image.paste(src, (x,y), mask) where the mask is
// src's own alpha (the editorial_big shadow paste).
func PasteWithMask(dst *image.RGBA, src *image.NRGBA, x, y int) {
	rect := image.Rect(x, y, x+src.Bounds().Dx(), y+src.Bounds().Dy())
	draw.DrawMask(dst, rect, src, image.Point{}, src, image.Point{}, draw.Over)
}

// Colorize mirrors PIL ImageOps.colorize: maps gray 0->black, 128->mid,
// 255->white with piecewise-linear interpolation per channel.
func Colorize(gray *image.Gray, black, mid, white color.RGBA) *image.RGBA {
	var lut [3][256]uint8
	bs := [3]int{int(black.R), int(black.G), int(black.B)}
	ms := [3]int{int(mid.R), int(mid.G), int(mid.B)}
	ws := [3]int{int(white.R), int(white.G), int(white.B)}
	for ch := 0; ch < 3; ch++ {
		for v := 0; v < 256; v++ {
			if v < 128 {
				lut[ch][v] = uint8(bs[ch] + (ms[ch]-bs[ch])*v/128)
			} else {
				lut[ch][v] = uint8(ms[ch] + (ws[ch]-ms[ch])*(v-128)/127)
			}
		}
	}
	out := image.NewRGBA(gray.Bounds())
	for i, v := range gray.Pix {
		out.Pix[i*4] = lut[0][v]
		out.Pix[i*4+1] = lut[1][v]
		out.Pix[i*4+2] = lut[2][v]
		out.Pix[i*4+3] = 0xFF
	}
	return out
}

// NoiseGray mirrors PIL Image.effect_noise(size, sigma): gaussian noise
// centered on 128.
func NoiseGray(w, h int, sigma float64, rng *rand.Rand) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, w, h))
	for i := range out.Pix {
		out.Pix[i] = clamp(128 + rng.NormFloat64()*sigma)
	}
	return out
}

var findEdgesKernel = [9]int{-1, -1, -1, -1, 8, -1, -1, -1, -1}

// FindEdgesMean applies PIL's FIND_EDGES convolution to the region of gray
// and returns the mean of the result (used by the backdrop scorer on the
// cropped left-bottom title area, exactly like enhance.py).
func FindEdgesMean(gray *image.Gray) float64 {
	w, h := gray.Bounds().Dx(), gray.Bounds().Dy()
	var sum float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var acc int
			for ky := -1; ky <= 1; ky++ {
				for kx := -1; kx <= 1; kx++ {
					xx, yy := x+kx, y+ky
					if xx < 0 {
						xx = 0
					}
					if xx >= w {
						xx = w - 1
					}
					if yy < 0 {
						yy = 0
					}
					if yy >= h {
						yy = h - 1
					}
					acc += findEdgesKernel[(ky+1)*3+(kx+1)] * int(gray.Pix[yy*gray.Stride+xx])
				}
			}
			if acc < 0 {
				acc = 0
			}
			if acc > 255 {
				acc = 255
			}
			sum += float64(acc)
		}
	}
	return sum / float64(w*h)
}

// CropGray returns the sub-rectangle of a gray image (PIL crop).
func CropGray(gray *image.Gray, r image.Rectangle) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), gray, r.Min, draw.Src)
	return out
}

// RoundedCorners applies a transparent rounded-corner mask like the Python
// styles' _apply_rounded_corners (radius in pixels). Edges get 1px
// anti-aliasing where PIL has none — a deliberate, invisible-at-a-glance
// improvement.
func RoundedCorners(img *image.RGBA, radius int) *image.RGBA {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	out := image.NewRGBA(img.Bounds())
	r := float64(radius)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Distance outside the rounded rect (negative inside).
			cx := math.Max(math.Max(r-float64(x)-0.5, float64(x)+0.5-(float64(w)-r)), 0)
			cy := math.Max(math.Max(r-float64(y)-0.5, float64(y)+0.5-(float64(h)-r)), 0)
			d := math.Hypot(cx, cy) - r
			var alpha float64
			switch {
			case d <= -0.5:
				alpha = 1
			case d >= 0.5:
				alpha = 0
			default:
				alpha = 0.5 - d
			}
			si := y*img.Stride + x*4
			di := y*out.Stride + x*4
			out.Pix[di] = img.Pix[si]
			out.Pix[di+1] = img.Pix[si+1]
			out.Pix[di+2] = img.Pix[si+2]
			out.Pix[di+3] = uint8(alpha*255 + 0.5)
		}
	}
	return out
}

// EllipseMaskGray draws a filled ellipse (PIL ImageDraw.ellipse) on a black
// gray canvas; used for vignettes before blurring.
func EllipseMaskGray(w, h int, rect image.Rectangle) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, w, h))
	cx := float64(rect.Min.X+rect.Max.X) / 2
	cy := float64(rect.Min.Y+rect.Max.Y) / 2
	rx := float64(rect.Dx()) / 2
	ry := float64(rect.Dy()) / 2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx := (float64(x) + 0.5 - cx) / rx
			dy := (float64(y) + 0.5 - cy) / ry
			if dx*dx+dy*dy <= 1 {
				out.Pix[y*out.Stride+x] = 0xFF
			}
		}
	}
	return out
}

// Compile-time guards for optional stdlib decoders.
var (
	_ = jpeg.Decode
	_ = gif.Decode
	_ = png.Decode
)
