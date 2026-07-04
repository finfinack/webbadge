package nfcink

import "encoding/hex"

// Default screen geometry, overridden by DeviceCfg once the badge's config
// TLV has been read.
const (
	ScreenWidth  = 240
	ScreenHeight = 416
)

// Colour indices, matching pkg/badgeimage's palette codes exactly
// (black=0, white=1, yellow=2, red=3).
const (
	ColorBlack  = 0
	ColorWhite  = 1
	ColorYellow = 2
	ColorRed    = 3
)

// Raw APDUs, hex-decoded once at init.
var (
	apduReadConfig    = mustHex("00D1000000")
	apduReadImageInfo = mustHex("00EB000002")
	apduDeviceCheck   = mustHex("F0D8000005000000000E")
	apduPollRefresh   = mustHex("F0DE000001")
)

// Status words.
var (
	swOK          = [2]byte{0x90, 0x00}
	swColorScreen = [2]byte{0x68, 0xC6} // caller should escalate refresh stage
	sw68CA        = [2]byte{0x68, 0xCA} // device busy
	sw6986        = [2]byte{0x69, 0x86} // command not allowed; retry from start
	sw698A        = [2]byte{0x69, 0x8A} // device hardware error (terminal)
)

func mustHex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return decoded
}
