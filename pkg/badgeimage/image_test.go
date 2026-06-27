package badgeimage

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestPrepareSolidColors(t *testing.T) {
	tests := []struct {
		name       string
		color      color.NRGBA
		packedByte byte
	}{
		{name: "black", color: color.NRGBA{A: 255}, packedByte: 0x00},
		{name: "white", color: color.NRGBA{R: 255, G: 255, B: 255, A: 255}, packedByte: 0x55},
		{name: "yellow", color: color.NRGBA{R: 255, G: 255, A: 255}, packedByte: 0xaa},
		{name: "red", color: color.NRGBA{R: 255, A: 255}, packedByte: 0xff},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := encodePNG(t, image.NewUniform(test.color), image.Rect(0, 0, 10, 10))

			result, err := Prepare(input, FitCover)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if len(result.Packed) != PackedSize {
				t.Fatalf("packed size = %d, want %d", len(result.Packed), PackedSize)
			}
			for index, value := range result.Packed {
				if value != test.packedByte {
					t.Fatalf("packed[%d] = %#02x, want %#02x", index, value, test.packedByte)
				}
			}

			preview, err := png.Decode(bytes.NewReader(result.Preview))
			if err != nil {
				t.Fatalf("decode preview: %v", err)
			}
			if preview.Bounds().Dx() != Width || preview.Bounds().Dy() != Height {
				t.Fatalf("preview bounds = %v, want %dx%d", preview.Bounds(), Width, Height)
			}
		})
	}
}

func TestPrepareValidation(t *testing.T) {
	validPNG := encodePNG(t, image.NewUniform(color.White), image.Rect(0, 0, 2, 2))
	tests := []struct {
		name  string
		input []byte
		fit   Fit
	}{
		{name: "invalid fit", input: validPNG, fit: "stretch"},
		{name: "not an image", input: []byte("not an image"), fit: FitContain},
		{name: "empty image", input: nil, fit: FitContain},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Prepare(test.input, test.fit); err == nil {
				t.Fatal("Prepare() error = nil, want validation error")
			}
		})
	}
}

func TestPrepareJPEG(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for index := 0; index < len(source.Pix); index++ {
		source.Pix[index] = 255
	}
	var input bytes.Buffer
	if err := jpeg.Encode(&input, source, nil); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	result, err := Prepare(input.Bytes(), FitContain)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(result.Packed) != PackedSize {
		t.Fatalf("packed size = %d, want %d", len(result.Packed), PackedSize)
	}
}

func encodePNG(t *testing.T, source image.Image, bounds image.Rectangle) []byte {
	t.Helper()

	canvas := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			canvas.Set(x, y, source.At(x, y))
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, canvas); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return output.Bytes()
}
