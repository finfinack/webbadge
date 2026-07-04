package nfcbadge

import (
	"image"
	"image/color"
	"testing"
)

func TestNearestE5Color(t *testing.T) {
	tests := []struct {
		name string
		in   color.RGBA
		want uint8
	}{
		{name: "black", in: color.RGBA{A: 255}, want: 0},
		{name: "white", in: color.RGBA{R: 255, G: 255, B: 255, A: 255}, want: 1},
		{name: "red", in: color.RGBA{R: 255, A: 255}, want: 3},
		{name: "yellow", in: color.RGBA{R: 255, G: 255, A: 255}, want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := nearestE5Color(test.in.RGBA())
			if got != test.want {
				t.Fatalf("nearestE5Color() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestEncode397ValidatesDimensions(t *testing.T) {
	_, err := encode397(image.NewRGBA(image.Rect(0, 0, 10, 10)))
	if err == nil {
		t.Fatal("encode397() error = nil, want dimension error")
	}
}

func TestEncode397Length(t *testing.T) {
	imageValue := image.NewRGBA(image.Rect(0, 0, nfca397Width, nfca397Height))
	encoded, err := encode397(imageValue)
	if err != nil {
		t.Fatalf("encode397() error = %v", err)
	}
	want := nfca397Width * nfca397Height / 4
	if len(encoded) != want {
		t.Fatalf("len(encode397()) = %d, want %d", len(encoded), want)
	}
}

func TestLZ4LiteralBlock(t *testing.T) {
	tests := []struct {
		name       string
		sourceSize int
		wantPrefix []byte
	}{
		{name: "short", sourceSize: 3, wantPrefix: []byte{0x30}},
		{name: "fifteen", sourceSize: 15, wantPrefix: []byte{0xF0, 0x00}},
		{name: "long", sourceSize: 270, wantPrefix: []byte{0xF0, 0xFF, 0x00}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			block := lz4LiteralBlock(make([]byte, test.sourceSize))
			if len(block) < len(test.wantPrefix) {
				t.Fatalf("block length = %d, want at least %d", len(block), len(test.wantPrefix))
			}
			for index := range test.wantPrefix {
				if block[index] != test.wantPrefix[index] {
					t.Fatalf("block[%d] = %02X, want %02X", index, block[index], test.wantPrefix[index])
				}
			}
		})
	}
}
