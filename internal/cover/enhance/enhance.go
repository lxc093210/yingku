// Package enhance ports style/enhance.py: adaptive backdrop enhancement and
// the technical candidate scoring used to rank the 12-image pool.
//
// Scoring (score_backdrop_bytes in Python):
//   - decode, convert to L, resize to 96x54
//   - start at 100; mean < 100 loses (100-mean)*1.6; mean > 170 loses
//     (mean-170)*1.0; stddev < 42 loses (42-std)*1.4
//   - the left-bottom title area (crop (0,27,32,54)) goes through FIND_EDGES;
//     edge mean > 24 loses (edge_mean-24)*1.1
//
// Enhancement (enhance_backdrop): if the 64x36 luminance mean is below 100,
// brighten by min(1.32, 1+(100-mean)/190) and raise contrast by 1.06; every
// image then gets UnsharpMask(radius=2, percent=68, threshold=2).
package enhance

import (
	"image"
	"math"

	xdraw "golang.org/x/image/draw"

	"yingku/internal/cover/imgx"
)

// DarkThreshold is Python's DARK_THRESHOLD.
const DarkThreshold = 100.0

func meanLuminance(img image.Image) float64 {
	small := imgx.Resize(img, 64, 36, xdraw.ApproxBiLinear)
	mean, _ := imgx.MeanStd(imgx.Gray(small))
	return mean
}

// EnhanceBackdrop ports enhance_backdrop.
func EnhanceBackdrop(img *image.RGBA) *image.RGBA {
	out := img
	if meanLuminance(img) < DarkThreshold {
		mean := meanLuminance(img)
		factor := 1.0 + (DarkThreshold-mean)/190.0
		if factor > 1.32 {
			factor = 1.32
		}
		out = imgx.AdjustBrightness(out, factor)
		out = imgx.AdjustContrast(out, 1.06)
	}
	return imgx.UnsharpMask(out, 2, 68, 2)
}

// ScoreBackdropBytes ports score_backdrop_bytes. Undecodable content scores
// negative infinity, like a failed download in the Python ranker (which
// leaves such items at -inf).
func ScoreBackdropBytes(content []byte) float64 {
	img, err := imgx.DecodeBytes(content)
	if err != nil {
		return math.Inf(-1)
	}
	return ScoreImage(img)
}

// ScoreImage is ScoreBackdropBytes on an already-decoded image.
func ScoreImage(img image.Image) float64 {
	small := imgx.Resize(img, 96, 54, xdraw.ApproxBiLinear)
	gray := imgx.Gray(small)
	mean, std := imgx.MeanStd(gray)

	score := 100.0
	if mean < 100 {
		score -= (100 - mean) * 1.6
	}
	if mean > 170 {
		score -= (mean - 170) * 1.0
	}
	if std < 42 {
		score -= (42 - std) * 1.4
	}
	leftBottom := imgx.CropGray(gray, image.Rect(0, 27, 32, 54))
	edgeMean := imgx.FindEdgesMean(leftBottom)
	if edgeMean > 24 {
		score -= (edgeMean - 24) * 1.1
	}
	return score
}
