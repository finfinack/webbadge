package nfcink

// DeviceCfg is the device configuration parsed from concatenating the
// responses of the 00D1000000 (TLV) and F0D8000005000000000E (PIN/4-color
// marker) APDUs. Only fields the write/refresh path needs are kept; see
// old/nfcink/nfcink/config.py for the full original parser (manufacturer
// info, app ID, UUID, user-data size, PIN and compression flags, etc. are
// intentionally not ported).
type DeviceCfg struct {
	ScreenWidth     int
	ScreenHeight    int
	ColorCount      int
	ColorDic        map[byte]string // colour index -> wire-encoding bits, e.g. "00"
	PictureCapacity int
	FlipVertical    bool // populated separately, from cmd_read_image_info
}

// paletteFourColor is the canonical wire encoding forced by the "4_color
// Screen" trailer marker.
var paletteFourColor = map[byte]string{
	ColorBlack:  "00",
	ColorWhite:  "01",
	ColorRed:    "11",
	ColorYellow: "10",
}

var fourColorMarker = []byte("4_color Screen")

// colorCodeInfo maps the A0-byte-2 colour code to (colorCount, hasRed, hasYellow).
var colorCodeInfo = map[byte][3]int{
	0x20: {2, 0, 0}, // 2-colour B/W
	0x30: {3, 1, 0}, // 3-colour B/W/R
	0x31: {3, 0, 1}, // 3-colour B/W/Y
}

// ParseDeviceCfg parses the concatenation of both APDU responses (TLV +
// PIN/4-color-marker trailer). resp2 may be empty if the second APDU
// failed; parsing degrades gracefully in that case.
func ParseDeviceCfg(resp1, resp2 []byte) DeviceCfg {
	data := append(append([]byte(nil), resp1...), resp2...)
	cfg := DeviceCfg{
		ScreenWidth:  ScreenWidth,
		ScreenHeight: ScreenHeight,
		ColorCount:   4,
		ColorDic:     copyColorDic(paletteFourColor),
	}
	if len(data) < 2 || data[0] != 0xA0 {
		return cfg
	}

	pos := 0
	pos = consumeTag(data, pos, 0xA0, func(value []byte) { parseA0(&cfg, value) })
	pos = consumeTag(data, pos, 0xA1, func(value []byte) { parseA1(&cfg, value) })
	consumeTag(data, pos, 0xB1, func(value []byte) { parseB1(&cfg, value) })
	parseTrailer(&cfg, data)
	return cfg
}

func copyColorDic(source map[byte]string) map[byte]string {
	result := make(map[byte]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// consumeTag runs parser on the value bytes of one TLV entry at data[pos] if
// its tag matches, returning the offset of the next tag; otherwise it
// returns pos unchanged.
func consumeTag(data []byte, pos int, tag byte, parser func(value []byte)) int {
	if pos+1 >= len(data) || data[pos] != tag {
		return pos
	}
	length := int(data[pos+1])
	valueEnd := pos + 2 + length
	if valueEnd > len(data) {
		return pos
	}
	parser(data[pos+2 : valueEnd])
	return valueEnd
}

// parseA0 reads manufacturer code, colour code, height (BE), width (BE).
func parseA0(cfg *DeviceCfg, value []byte) {
	if len(value) >= 3 {
		colorCode := value[2]
		if info, ok := colorCodeInfo[colorCode]; ok {
			cfg.ColorCount = info[0]
		}
	}
	if len(value) >= 5 {
		cfg.ScreenHeight = int(value[3])<<8 | int(value[4])
	}
	if len(value) >= 7 {
		cfg.ScreenWidth = int(value[5])<<8 | int(value[6])
	}
}

// parseA1 reads refresh scan, size + colour count, and per-colour wire
// encoding records.
func parseA1(cfg *DeviceCfg, value []byte) {
	if len(value) < 2 {
		return
	}
	colorCount := int(value[1] & 0x0F)
	cfg.ColorCount = colorCount

	nBits := 2
	if colorCount == 2 {
		nBits = 1
	}
	shift := (3 - colorCount) + 3
	mask := byte(1<<nBits) - 1
	palette := make(map[byte]string)

	for index := 0; index < colorCount; index++ {
		if 2+index >= len(value) {
			break
		}
		record := value[2+index]
		colorID := (record >> 5) & 0x07
		encoding := formatBits((record>>shift)&mask, nBits)
		switch colorID {
		case 0:
			palette[ColorBlack] = encoding
		case 1:
			palette[ColorWhite] = encoding
		case 2:
			palette[ColorRed] = encoding
		case 3:
			palette[ColorYellow] = encoding
		}
	}
	if len(palette) > 0 {
		cfg.ColorDic = palette
	}
}

func parseB1(cfg *DeviceCfg, value []byte) {
	if len(value) >= 1 {
		cfg.PictureCapacity = int(value[0])
	}
}

// parseTrailer applies the "4_color Screen" override: SW=9000 with the 14
// bytes preceding it equal to the marker forces the canonical 4-colour
// palette and halves the reported screen height (mirrors config.py's
// _parse_trailer).
func parseTrailer(cfg *DeviceCfg, data []byte) {
	if len(data) < 16 {
		return
	}
	sw := data[len(data)-2:]
	if sw[0] != swOK[0] || sw[1] != swOK[1] {
		return
	}
	marker := data[len(data)-16 : len(data)-2]
	if string(marker) == string(fourColorMarker) {
		cfg.ColorCount = 4
		cfg.ScreenHeight /= 2
		cfg.ColorDic = copyColorDic(paletteFourColor)
	}
}

func formatBits(value byte, bits int) string {
	result := make([]byte, bits)
	for index := bits - 1; index >= 0; index-- {
		if value&1 == 1 {
			result[index] = '1'
		} else {
			result[index] = '0'
		}
		value >>= 1
	}
	return string(result)
}
