package nfcbadge

import (
	"errors"
	"image"
)

const (
	nfca397Width  = 460
	nfca397Height = 768
	imageBlockLen = 7360
)

func encode397(imageValue image.Image) ([]byte, error) {
	bounds := imageValue.Bounds()
	if bounds.Dx() != nfca397Width || bounds.Dy() != nfca397Height {
		return nil, errors.New("nfc-a-397 requires an image exactly 460x768 pixels")
	}

	// Match Android's -90 degree bitmap rotation.
	rotatedWidth := bounds.Dy()
	rotatedHeight := bounds.Dx()
	rotated := make([]uint8, rotatedWidth*rotatedHeight)
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			destinationX := y
			destinationY := bounds.Dx() - 1 - x
			rotated[destinationY*rotatedWidth+destinationX] = nearestE5Color(imageValue.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA())
		}
	}

	// Match ImageOp.getImageBytesE51's second coordinate transform.
	transformed := make([]uint8, len(rotated))
	for index, color := range rotated {
		x := index % rotatedWidth
		y := index / rotatedWidth
		transformed[((rotatedWidth-1-x)*rotatedHeight)+y] = color
	}

	encoded := make([]byte, len(transformed)/4)
	for index := 0; index < len(transformed); index += 4 {
		encoded[index/4] = transformed[index]<<6 |
			transformed[index+1]<<4 |
			transformed[index+2]<<2 |
			transformed[index+3]
	}
	return encoded, nil
}

func nearestE5Color(red, green, blue, _ uint32) uint8 {
	r := int(red >> 8)
	g := int(green >> 8)
	b := int(blue >> 8)
	distances := [...]int{
		r*r + g*g + b*b,
		square(r-255) + square(g-255) + square(b-255),
		square(r-255) + g*g + b*b,
		square(r-255) + square(g-255) + b*b,
	}
	minimumIndex := 0
	for index := 1; index < len(distances); index++ {
		if distances[index] < distances[minimumIndex] {
			minimumIndex = index
		}
	}
	switch minimumIndex {
	case 1:
		return 1 // white
	case 2:
		return 3 // red
	case 3:
		return 2 // yellow
	default:
		return 0 // black
	}
}

func square(value int) int {
	return value * value
}

// lz4LiteralBlock creates a valid raw LZ4 block consisting of one literal-only
// sequence. It is larger than high-compression output but deterministic and
// avoids depending on an incompatible framed LZ4 encoder.
func lz4LiteralBlock(source []byte) []byte {
	literalLength := len(source)
	tokenLength := literalLength
	if tokenLength > 15 {
		tokenLength = 15
	}
	result := make([]byte, 0, literalLength+(literalLength/255)+2)
	result = append(result, byte(tokenLength<<4))
	if literalLength >= 15 {
		remaining := literalLength - 15
		for remaining >= 255 {
			result = append(result, 255)
			remaining -= 255
		}
		result = append(result, byte(remaining))
	}
	result = append(result, source...)
	return result
}

func compress397(encoded []byte) [][]byte {
	blockCount := (len(encoded) + imageBlockLen - 1) / imageBlockLen
	blocks := make([][]byte, 0, blockCount)
	for offset := 0; offset < len(encoded); offset += imageBlockLen {
		end := offset + imageBlockLen
		if end > len(encoded) {
			end = len(encoded)
		}
		blocks = append(blocks, lz4LiteralBlock(encoded[offset:end]))
	}
	return blocks
}
