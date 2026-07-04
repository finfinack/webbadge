// Package badgeimage prepares images for the four-color e-ink badge.
package badgeimage

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"math"
)

const (
	// Width is Hybrid397's pixel width, kept for existing BLE code and tests.
	Width = 460
	// Height is Hybrid397's pixel height, kept for existing BLE code and tests.
	Height = 768
	// PackedSize is the number of bytes in Hybrid397's packed two-bit image.
	PackedSize = Width * Height / 4

	maxSourceDimension = 12_000
	maxSourcePixels    = 20_000_000
)

// Profile describes one physical badge's target geometry.
type Profile struct {
	ID     string
	Label  string
	Width  int
	Height int
}

// Hybrid397 is the 3.97in four-colour hybrid BLE/NFC badge.
var Hybrid397 = Profile{ID: "hybrid-397", Label: "3.97in 4-colour hybrid", Width: Width, Height: Height}

// NfcInk240x416 is the 240x416 four-colour NFC-only badge.
var NfcInk240x416 = Profile{ID: "nfcink-240x416", Label: "nfcink 4-colour (240x416)", Width: 240, Height: 416}

// Profiles lists every supported badge target.
var Profiles = []Profile{Hybrid397, NfcInk240x416}

// ProfileByID looks up a profile by its ID, as sent by the frontend.
func ProfileByID(id string) (Profile, bool) {
	for _, profile := range Profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return Profile{}, false
}

// Fit controls how a source image is placed on the badge canvas.
type Fit string

const (
	// FitContain shows the whole image and adds white space as needed.
	FitContain Fit = "contain"
	// FitCover fills the canvas and crops overflow.
	FitCover Fit = "cover"
)

// Result contains both a human-readable preview and badge-ready pixel data.
type Result struct {
	Preview []byte
	Packed  []byte
}

type paletteColor struct {
	r, g, b float64
	code    byte
}

var palette = [...]paletteColor{
	{r: 0, g: 0, b: 0, code: 0},
	{r: 255, g: 255, b: 255, code: 1},
	{r: 255, g: 0, b: 0, code: 3},
	{r: 255, g: 255, b: 0, code: 2},
}

// Prepare decodes, resizes, dithers, and packs PNG or JPEG data for profile.
func Prepare(input []byte, fit Fit, profile Profile) (Result, error) {
	if fit != FitContain && fit != FitCover {
		return Result{}, fmt.Errorf("unsupported fit mode %q", fit)
	}
	if profile.Width <= 0 || profile.Height <= 0 {
		return Result{}, fmt.Errorf("unsupported badge profile %q", profile.ID)
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(input))
	if err != nil {
		return Result{}, fmt.Errorf("read image metadata: %w", err)
	}
	if format != "png" && format != "jpeg" {
		return Result{}, fmt.Errorf("unsupported image format %q", format)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxSourceDimension || config.Height > maxSourceDimension ||
		config.Width > maxSourcePixels/config.Height {
		return Result{}, errors.New("image dimensions are too large")
	}

	source, decodedFormat, err := image.Decode(bytes.NewReader(input))
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}
	if decodedFormat != format {
		return Result{}, errors.New("image format changed while decoding")
	}

	canvas := resize(source, fit, profile)
	preview, packed := ditherAndPack(canvas, profile)

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, preview); err != nil {
		return Result{}, fmt.Errorf("encode preview: %w", err)
	}
	return Result{Preview: encoded.Bytes(), Packed: packed}, nil
}

func resize(source image.Image, fit Fit, profile Profile) *image.NRGBA {
	width, height := profile.Width, profile.Height
	output := image.NewNRGBA(image.Rect(0, 0, width, height))
	for index := 0; index < len(output.Pix); index += 4 {
		output.Pix[index] = 255
		output.Pix[index+1] = 255
		output.Pix[index+2] = 255
		output.Pix[index+3] = 255
	}

	bounds := source.Bounds()
	sourceWidth := bounds.Dx()
	sourceHeight := bounds.Dy()
	scaleX := float64(width) / float64(sourceWidth)
	scaleY := float64(height) / float64(sourceHeight)
	scale := math.Min(scaleX, scaleY)
	if fit == FitCover {
		scale = math.Max(scaleX, scaleY)
	}

	drawWidth := float64(sourceWidth) * scale
	drawHeight := float64(sourceHeight) * scale
	offsetX := (float64(width) - drawWidth) / 2
	offsetY := (float64(height) - drawHeight) / 2

	for y := 0; y < height; y++ {
		if float64(y)+0.5 < offsetY || float64(y)+0.5 >= offsetY+drawHeight {
			continue
		}
		sourceY := (float64(y)-offsetY+0.5)/scale - 0.5
		sourceY = min(max(sourceY, 0), float64(sourceHeight-1))
		for x := 0; x < width; x++ {
			if float64(x)+0.5 < offsetX || float64(x)+0.5 >= offsetX+drawWidth {
				continue
			}
			sourceX := (float64(x)-offsetX+0.5)/scale - 0.5
			sourceX = min(max(sourceX, 0), float64(sourceWidth-1))
			sampled := bilinear(source, float64(bounds.Min.X)+sourceX, float64(bounds.Min.Y)+sourceY)
			output.SetNRGBA(x, y, sampled)
		}
	}
	return output
}

func bilinear(source image.Image, x, y float64) color.NRGBA {
	bounds := source.Bounds()
	x0 := int(math.Floor(x))
	y0 := int(math.Floor(y))
	x1 := min(x0+1, bounds.Max.X-1)
	y1 := min(y0+1, bounds.Max.Y-1)
	x0 = max(x0, bounds.Min.X)
	y0 = max(y0, bounds.Min.Y)

	xWeight := x - math.Floor(x)
	yWeight := y - math.Floor(y)
	top := mixColor(color.NRGBAModel.Convert(source.At(x0, y0)).(color.NRGBA),
		color.NRGBAModel.Convert(source.At(x1, y0)).(color.NRGBA), xWeight)
	bottom := mixColor(color.NRGBAModel.Convert(source.At(x0, y1)).(color.NRGBA),
		color.NRGBAModel.Convert(source.At(x1, y1)).(color.NRGBA), xWeight)
	return mixColor(top, bottom, yWeight)
}

func mixColor(first, second color.NRGBA, weight float64) color.NRGBA {
	mix := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a)*(1-weight) + float64(b)*weight))
	}
	alpha := mix(first.A, second.A)
	if alpha == 255 {
		return color.NRGBA{
			R: mix(first.R, second.R),
			G: mix(first.G, second.G),
			B: mix(first.B, second.B),
			A: 255,
		}
	}

	// Composite transparent input onto the badge's white background.
	composite := func(a, b uint8) uint8 {
		value := float64(mix(a, b))
		return uint8(math.Round(value*float64(alpha)/255 + 255*(1-float64(alpha)/255)))
	}
	return color.NRGBA{
		R: composite(first.R, second.R),
		G: composite(first.G, second.G),
		B: composite(first.B, second.B),
		A: 255,
	}
}

func ditherAndPack(input *image.NRGBA, profile Profile) (*image.NRGBA, []byte) {
	width, height := profile.Width, profile.Height
	pixelCount := width * height
	work := make([]float64, pixelCount*3)
	codes := make([]byte, pixelCount)
	output := image.NewNRGBA(input.Bounds())
	for index := 0; index < pixelCount; index++ {
		pixelOffset := index * 4
		work[index*3] = float64(input.Pix[pixelOffset])
		work[index*3+1] = float64(input.Pix[pixelOffset+1])
		work[index*3+2] = float64(input.Pix[pixelOffset+2])
	}

	diffuse := func(index int, red, green, blue, factor float64) {
		if index < 0 || index >= pixelCount {
			return
		}
		work[index*3] += red * factor
		work[index*3+1] += green * factor
		work[index*3+2] += blue * factor
	}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := y*width + x
			selected := nearest(work[index*3], work[index*3+1], work[index*3+2])
			codes[index] = selected.code
			pixelOffset := index * 4
			output.Pix[pixelOffset] = uint8(selected.r)
			output.Pix[pixelOffset+1] = uint8(selected.g)
			output.Pix[pixelOffset+2] = uint8(selected.b)
			output.Pix[pixelOffset+3] = 255

			redError := work[index*3] - selected.r
			greenError := work[index*3+1] - selected.g
			blueError := work[index*3+2] - selected.b
			if x+1 < width {
				diffuse(index+1, redError, greenError, blueError, 7.0/16)
			}
			if y+1 < height {
				if x > 0 {
					diffuse(index+width-1, redError, greenError, blueError, 3.0/16)
				}
				diffuse(index+width, redError, greenError, blueError, 5.0/16)
				if x+1 < width {
					diffuse(index+width+1, redError, greenError, blueError, 1.0/16)
				}
			}
		}
	}

	packed := make([]byte, pixelCount/4)
	for outputPixel := 0; outputPixel < pixelCount; outputPixel++ {
		sourcePixel := pixelCount - 1 - outputPixel
		packed[outputPixel>>2] |= codes[sourcePixel] << ((3 - (outputPixel & 3)) * 2)
	}
	return output, packed
}

func nearest(red, green, blue float64) paletteColor {
	selected := palette[0]
	bestDistance := math.Inf(1)
	for _, candidate := range palette {
		redDelta := red - candidate.r
		greenDelta := green - candidate.g
		blueDelta := blue - candidate.b
		distance := redDelta*redDelta + greenDelta*greenDelta + blueDelta*blueDelta
		if distance < bestDistance {
			selected = candidate
			bestDistance = distance
		}
	}
	return selected
}
