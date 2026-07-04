package nfcink

import "image"

// nearestCode maps an exact quantized palette pixel back to a 0..3 colour
// code, using the same black/white/yellow/red distance test as
// pkg/badgeimage's own palette so the two agree on ties/rounding.
func nearestCode(red, green, blue uint32) byte {
	r := int(red >> 8)
	g := int(green >> 8)
	b := int(blue >> 8)
	distances := [4]int{
		r*r + g*g + b*b, // black
		square(r-255) + square(g-255) + square(b-255), // white
		square(r-255) + square(g-255) + b*b,           // yellow
		square(r-255) + g*g + b*b,                     // red
	}
	best := 0
	for index := 1; index < len(distances); index++ {
		if distances[index] < distances[best] {
			best = index
		}
	}
	switch best {
	case 1:
		return ColorWhite
	case 2:
		return ColorYellow
	case 3:
		return ColorRed
	default:
		return ColorBlack
	}
}

func square(value int) int { return value * value }

// EncodeImage packs an already-quantized image (e.g. pkg/badgeimage's
// dithered preview, resized to cfg's geometry) into device bytes: an
// unconditional horizontal flip, an optional vertical flip
// (cfg.FlipVertical, from the badge's image-info APDU), then 4 colour
// codes per byte MSB-first using cfg.ColorDic. Mirrors image.py's
// quantise_image (flip steps) + pixels_to_bytes (packing).
func EncodeImage(img image.Image, cfg DeviceCfg) []byte {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	codes := make([]byte, width*height)
	for y := 0; y < height; y++ {
		sourceY := bounds.Min.Y + y
		if cfg.FlipVertical {
			sourceY = bounds.Min.Y + (height - 1 - y)
		}
		for x := 0; x < width; x++ {
			sourceX := bounds.Min.X + (width - 1 - x)
			r, g, b, _ := img.At(sourceX, sourceY).RGBA()
			codes[y*width+x] = nearestCode(r, g, b)
		}
	}
	return packCodes(codes, cfg.ColorDic)
}

func packCodes(codes []byte, colorDic map[byte]string) []byte {
	result := make([]byte, (len(codes)+3)/4)
	for index := range result {
		var value byte
		for slot := 0; slot < 4; slot++ {
			codeIndex := index*4 + slot
			code := byte(ColorBlack)
			if codeIndex < len(codes) {
				code = codes[codeIndex]
			}
			value <<= 2
			value |= parseBits(colorDic[code])
		}
		result[index] = value
	}
	return result
}

func parseBits(bits string) byte {
	var value byte
	for _, character := range bits {
		value <<= 1
		if character == '1' {
			value |= 1
		}
	}
	return value
}
