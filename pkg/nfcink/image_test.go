package nfcink

import (
	"image"
	"image/color"
	"testing"
)

func TestNearestCode(t *testing.T) {
	tests := []struct {
		name string
		in   color.RGBA
		want byte
	}{
		{name: "black", in: color.RGBA{A: 255}, want: ColorBlack},
		{name: "white", in: color.RGBA{R: 255, G: 255, B: 255, A: 255}, want: ColorWhite},
		{name: "yellow", in: color.RGBA{R: 255, G: 255, A: 255}, want: ColorYellow},
		{name: "red", in: color.RGBA{R: 255, A: 255}, want: ColorRed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, g, b, _ := test.in.RGBA()
			if got := nearestCode(r, g, b); got != test.want {
				t.Fatalf("nearestCode() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestEncodeImageSize(t *testing.T) {
	canvas := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for index := 0; index < len(canvas.Pix); index += 4 {
		canvas.Pix[index+3] = 255
	}
	cfg := DeviceCfg{ColorDic: paletteFourColor}
	encoded := EncodeImage(canvas, cfg)
	if want := 8 / 4; len(encoded) != want {
		t.Fatalf("len(encoded) = %d, want %d", len(encoded), want)
	}
}

func TestEncodeImageFlipsHorizontally(t *testing.T) {
	// Left pixel red, right pixel black; after the unconditional
	// horizontal flip, the packed left slot should be black (code 0)
	// and the right slot should be red (code 3, wire "11").
	canvas := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	canvas.Set(0, 0, color.NRGBA{R: 255, A: 255})
	canvas.Set(1, 0, color.NRGBA{A: 255})
	cfg := DeviceCfg{ColorDic: paletteFourColor}

	encoded := EncodeImage(canvas, cfg)
	if len(encoded) != 1 {
		t.Fatalf("len(encoded) = %d, want 1", len(encoded))
	}
	// Packed byte: 4 slots x 2 bits; only first 2 slots are real pixels
	// (black=00 then red=11), remaining 2 slots pad with black (00).
	want := byte(0b00_11_00_00)
	if encoded[0] != want {
		t.Fatalf("encoded[0] = %08b, want %08b", encoded[0], want)
	}
}
