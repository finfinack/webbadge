package nfcink

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	blockSize      = 2000 // D3 (LZO) block size, matches protocol.py.
	subChunkSize   = 250  // max payload bytes per D2/D3 APDU.
	max6986Retries = 5
)

// NfcInkDevice is the command-level interface to the nfcink e-ink NFC
// display, ported from old/nfcink/nfcink/protocol.py and runner.py.
//
// Note on flip_vertical: the Python driver's own `write` command never
// calls read_image_info (that APDU is only used by its diagnostic `read`
// command), so DeviceCfg.FlipVertical is always false in practice during a
// real write; this port matches that behavior by simply never populating
// it, rather than porting an APDU exchange nothing in the write path uses.
type NfcInkDevice struct {
	card     Card
	progress func(completed, total int)
}

// NewNfcInkDevice creates a device wrapping card. progress, if non-nil, is
// called with (completed, total) after each APDU exchange during an image
// write.
func NewNfcInkDevice(card Card, progress func(completed, total int)) *NfcInkDevice {
	return &NfcInkDevice{card: card, progress: progress}
}

func (device *NfcInkDevice) report(completed, total int) {
	if device.progress != nil {
		device.progress(completed, total)
	}
}

// ReadConfig reads and parses the device configuration: the TLV APDU
// (00D1000000) plus the best-effort PIN/4-color-marker check
// (F0D8000005000000000E, allowed to fail, matching read_config()'s
// graceful degradation in the Python driver).
func (device *NfcInkDevice) ReadConfig(ctx context.Context) (DeviceCfg, error) {
	resp1, err := device.card.Transmit(ctx, apduReadConfig)
	if err != nil {
		return DeviceCfg{}, fmt.Errorf("read device config: %w", err)
	}
	if len(resp1) < 4 {
		return DeviceCfg{}, fmt.Errorf("config read failed: %X", resp1)
	}
	resp2, err := device.card.Transmit(ctx, apduDeviceCheck)
	if err != nil {
		resp2 = nil
	}
	return ParseDeviceCfg(resp1, resp2), nil
}

// WriteImageD2 uploads image data using raw (uncompressed) F0D2 APDUs.
func (device *NfcInkDevice) WriteImageD2(ctx context.Context, imageData []byte, section int) error {
	if len(imageData) == 0 {
		return errors.New("nfcink: empty image data")
	}
	chunkCount := (len(imageData) + subChunkSize - 1) / subChunkSize
	seq := 0
	for offset := 0; offset < len(imageData); offset += subChunkSize {
		end := min(offset+subChunkSize, len(imageData))
		apdu := append([]byte{0xF0, 0xD2, byte(section), byte(seq), byte(end - offset)}, imageData[offset:end]...)
		response, err := device.card.Transmit(ctx, apdu)
		if err != nil {
			return fmt.Errorf("write D2 chunk %d: %w", seq, err)
		}
		if !checkSW(response, swOK) {
			return fmt.Errorf("write D2 chunk %d failed: SW=%X", seq, lastTwo(response))
		}
		seq++
		device.report(seq, chunkCount)
	}
	return nil
}

// WriteImageD3 uploads image data using LZO1X-compressed F0D3 APDUs, per
// _BLOCK_SIZE-byte (2000) blocks.
func (device *NfcInkDevice) WriteImageD3(ctx context.Context, imageData []byte, section int) error {
	if len(imageData) == 0 {
		return errors.New("nfcink: empty image data")
	}
	blockCount := (len(imageData) + blockSize - 1) / blockSize
	for blockIndex := 0; blockIndex*blockSize < len(imageData); blockIndex++ {
		start := blockIndex * blockSize
		end := min(start+blockSize, len(imageData))
		compressed := CompressLZO1X(imageData[start:end])
		if err := device.writeD3Block(ctx, compressed, section, blockIndex); err != nil {
			return err
		}
		device.report(blockIndex+1, blockCount)
	}
	return nil
}

func (device *NfcInkDevice) writeD3Block(ctx context.Context, compressed []byte, section, blockIndex int) error {
	length := len(compressed)
	if length <= subChunkSize {
		return device.writeD3SubChunk(ctx, section, blockIndex, 0, compressed, true)
	}
	fullChunks := length / subChunkSize
	remainder := length % subChunkSize
	for subIndex := 0; subIndex < fullChunks; subIndex++ {
		offset := subIndex * subChunkSize
		isLast := remainder == 0 && subIndex == fullChunks-1
		if err := device.writeD3SubChunk(ctx, section, blockIndex, subIndex, compressed[offset:offset+subChunkSize], isLast); err != nil {
			return err
		}
	}
	if remainder > 0 {
		offset := fullChunks * subChunkSize
		if err := device.writeD3SubChunk(ctx, section, blockIndex, fullChunks, compressed[offset:], true); err != nil {
			return err
		}
	}
	return nil
}

func (device *NfcInkDevice) writeD3SubChunk(ctx context.Context, section, blockIndex, subIndex int, data []byte, isLast bool) error {
	p2 := byte(0)
	if isLast {
		p2 = 1
	}
	apdu := make([]byte, 0, 7+len(data))
	apdu = append(apdu, 0xF0, 0xD3, byte(section), p2, byte(len(data)+2), byte(blockIndex), byte(subIndex))
	apdu = append(apdu, data...)
	response, err := device.card.Transmit(ctx, apdu)
	if err != nil {
		return fmt.Errorf("write D3 block %d sub-chunk %d: %w", blockIndex, subIndex, err)
	}
	if !checkSW(response, swOK) {
		return fmt.Errorf("write D3 block %d sub-chunk %d failed: SW=%X", blockIndex, subIndex, lastTwo(response))
	}
	return nil
}

// Refresh triggers the badge's screen refresh and waits for it to
// complete: a direct port of runner.py's start_refresh / _send_refresh /
// _poll_once state machine (the 68C6 escalation to the ~15s F0D485
// waveform computation, 6986 retry-from-start up to 5 times, 68CA
// busy-retry-once, 698A terminal hardware error).
//
// Unlike the Python CLI, a transport error here (the NFC session
// dropping) is treated as a hard failure for this attempt rather than
// reconnecting and re-uploading the whole image: this is a single-shot
// web write, and that matches how the existing nfca397 writer and the BLE
// transfer already handle transient failures -- the user retries the
// write from the browser.
func (device *NfcInkDevice) Refresh(ctx context.Context, section int) error {
	return device.sendRefresh(ctx, section, false, false, max6986Retries)
}

func (device *NfcInkDevice) sendRefresh(ctx context.Context, section int, retriedOnce, useAlt bool, retries6986 int) error {
	response, err := device.refreshTransmit(ctx, section, useAlt)
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}

	sw := statusOf(response)
	data := dataOf(response)
	switch {
	case sw == sw698A:
		return fmt.Errorf("refresh: device hardware error (SW=%02X%02X)", sw[0], sw[1])

	case sw == sw6986:
		if retries6986 > 0 {
			return device.sendRefresh(ctx, section, false, false, retries6986-1)
		}
		return fmt.Errorf("refresh: device rejected refresh after %d retries (SW=6986)", max6986Retries)

	case sw == swColorScreen:
		if !retriedOnce {
			if err := sleepContext(ctx, 250*time.Millisecond); err != nil {
				return err
			}
			return device.sendRefresh(ctx, section, true, false, retries6986)
		}
		if !useAlt {
			return device.sendRefresh(ctx, section, true, true, retries6986)
		}
		return errors.New("refresh: rejected after alternate refresh (SW=68C6)")

	case sw == swOK && len(data) > 0 && data[0] == 0x01:
		return nil // device drawing autonomously

	case sw == swOK:
		return device.pollRefresh(ctx, section, retries6986)

	case sw == sw68CA:
		retryResponse, err := device.refreshTransmit(ctx, section, useAlt)
		if err != nil {
			return fmt.Errorf("refresh: %w", err)
		}
		if statusOf(retryResponse) == swOK && len(retryResponse) == 2 {
			return nil
		}
		return fmt.Errorf("refresh: failed after busy retry (SW=%X)", lastTwo(retryResponse))

	default:
		return fmt.Errorf("refresh: unexpected status (SW=%X)", lastTwo(response))
	}
}

func (device *NfcInkDevice) pollRefresh(ctx context.Context, section, retries6986 int) error {
	response, err := device.card.Transmit(ctx, apduPollRefresh)
	if err != nil {
		// A drop right after a successful refresh-init still means the
		// device is drawing autonomously; mirrors runner.py's _poll_once.
		return nil
	}
	sw := statusOf(response)
	data := dataOf(response)
	if sw == swOK && (len(data) == 0 || data[0] == 0x00 || data[0] == 0x01) {
		return nil
	}
	if sw == swColorScreen {
		return device.sendRefresh(ctx, section, false, false, retries6986)
	}
	return fmt.Errorf("refresh poll error (SW=%X)", lastTwo(response))
}

func (device *NfcInkDevice) refreshTransmit(ctx context.Context, section int, useAlt bool) ([]byte, error) {
	p1 := byte(0x05)
	if useAlt {
		p1 = 0x85
	}
	apdu := []byte{0xF0, 0xD4, p1, byte(section & 0x7F), 0x00}
	return device.card.Transmit(ctx, apdu)
}

func checkSW(response []byte, want [2]byte) bool {
	return statusOf(response) == want
}

func statusOf(response []byte) [2]byte {
	if len(response) < 2 {
		return [2]byte{}
	}
	return [2]byte{response[len(response)-2], response[len(response)-1]}
}

func dataOf(response []byte) []byte {
	if len(response) < 2 {
		return nil
	}
	return response[:len(response)-2]
}

func lastTwo(response []byte) []byte {
	if len(response) < 2 {
		return response
	}
	return response[len(response)-2:]
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("operation cancelled: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
