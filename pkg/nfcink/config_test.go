package nfcink

import "testing"

func TestParseDeviceCfgDefaultsOnEmptyResponse(t *testing.T) {
	cfg := ParseDeviceCfg(nil, nil)
	if cfg.ScreenWidth != ScreenWidth || cfg.ScreenHeight != ScreenHeight {
		t.Fatalf("geometry = %dx%d, want %dx%d", cfg.ScreenWidth, cfg.ScreenHeight, ScreenWidth, ScreenHeight)
	}
	if cfg.ColorCount != 4 {
		t.Fatalf("ColorCount = %d, want 4", cfg.ColorCount)
	}
}

func TestParseDeviceCfgAppliesFourColorMarker(t *testing.T) {
	// A0 TLV: manufacturer=0x00, unused byte, color_code=0x31 (3-colour B/W/Y),
	// height=832 (BE), width=240 (BE).
	a0Value := []byte{0x00, 0x00, 0x31, 0x03, 0x40, 0x00, 0xF0}
	resp1 := append([]byte{0xA0, byte(len(a0Value))}, a0Value...)
	resp1 = append(resp1, swOK[0], swOK[1])

	resp2 := append(append([]byte{}, fourColorMarker...), swOK[0], swOK[1])

	cfg := ParseDeviceCfg(resp1, resp2)
	if cfg.ColorCount != 4 {
		t.Fatalf("ColorCount = %d, want 4 (marker override)", cfg.ColorCount)
	}
	if cfg.ScreenHeight != 416 {
		t.Fatalf("ScreenHeight = %d, want 416 (halved by marker override)", cfg.ScreenHeight)
	}
	if cfg.ScreenWidth != 240 {
		t.Fatalf("ScreenWidth = %d, want 240", cfg.ScreenWidth)
	}
	want := map[byte]string{ColorBlack: "00", ColorWhite: "01", ColorRed: "11", ColorYellow: "10"}
	for code, bits := range want {
		if cfg.ColorDic[code] != bits {
			t.Errorf("ColorDic[%d] = %q, want %q", code, cfg.ColorDic[code], bits)
		}
	}
}

func TestParseDeviceCfgParsesA1Palette(t *testing.T) {
	// A1 TLV: refresh_scan=0, size|color_count=0x04 (color_count=4),
	// per-colour records: black=0x00 (id=0,bits=00), white=0x3C (id=1,bits=01<<... )
	// color_id in bits 7-5, encoding in bits (shift) with n_bits=2, shift=(3-4)+3=2.
	shift := 2
	record := func(colorID, bits byte) byte {
		return (colorID << 5) | (bits << shift)
	}
	a1Value := []byte{
		0x00, 0x04,
		record(0, 0b00), // black -> 00
		record(1, 0b01), // white -> 01
		record(2, 0b11), // red   -> 11
		record(3, 0b10), // yellow-> 10
	}
	resp1 := append([]byte{0xA0, 0x01, 0x00}, append([]byte{0xA1, byte(len(a1Value))}, a1Value...)...)
	resp1 = append(resp1, swOK[0], swOK[1])

	cfg := ParseDeviceCfg(resp1, nil)
	want := map[byte]string{ColorBlack: "00", ColorWhite: "01", ColorRed: "11", ColorYellow: "10"}
	for code, bits := range want {
		if cfg.ColorDic[code] != bits {
			t.Errorf("ColorDic[%d] = %q, want %q", code, cfg.ColorDic[code], bits)
		}
	}
	if cfg.ColorCount != 4 {
		t.Fatalf("ColorCount = %d, want 4", cfg.ColorCount)
	}
}
