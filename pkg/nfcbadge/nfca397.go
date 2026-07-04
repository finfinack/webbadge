package nfcbadge

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"time"
)

const (
	nfcaPassThroughMax = 255
	fifoPayloadMax     = 251
	imageSegmentMax    = 247
)

type nfcaTransport uint8

const (
	nfcaTransportUnknown nfcaTransport = iota
	nfcaTransportPassThrough
	nfcaTransportTransparent
)

// ProgressFunc receives completed and total work units.
type ProgressFunc func(completed, total int)

// NFCA397Writer writes a 460x768 four-colour image to the hybrid 3.97 badge.
type NFCA397Writer struct {
	card      Card
	progress  ProgressFunc
	transport nfcaTransport
}

type legacyControlCard interface {
	ControlLegacy(context.Context, []byte) ([]byte, error)
}

type controlCard interface {
	Control(context.Context, uint32, []byte) ([]byte, error)
}

const (
	cmIOCTLGetFeatureRequest = 0x42000D48
	featureCCIDEscapeCommand = 0x13
)

// NewNFCA397Writer creates a writer for the APK's NFC_BLE_397 profile.
func NewNFCA397Writer(card Card, progress ProgressFunc) *NFCA397Writer {
	return &NFCA397Writer{card: card, progress: progress}
}

// Write converts and transfers an image using the badge's raw NFC-A FIFO.
func (writer *NFCA397Writer) Write(ctx context.Context, imageValue image.Image) error {
	encoded, err := encode397(imageValue)
	if err != nil {
		return err
	}
	blocks := compress397(encoded)
	totalWrites := 1
	packagedBlocks := make([][]byte, len(blocks))
	for index, block := range blocks {
		packagedBlocks[index] = packageImageBlock(block)
		totalWrites += (len(block) / imageSegmentMax) + 1
	}

	completed := 0
	if err := writer.wakeBadge(ctx); err != nil {
		return fmt.Errorf("wake badge: %w", err)
	}
	completed++
	writer.report(completed, totalWrites)

	for blockIndex, packaged := range packagedBlocks {
		writes, err := writer.writeImageBlock(ctx, packaged)
		if err != nil {
			return fmt.Errorf("block %d: %w", blockIndex+1, err)
		}
		completed += writes
		writer.report(completed, totalWrites)
	}
	return nil
}

// wakeBadge mirrors the Android app's singleLow256Sleep: the wake poll is
// best-effort. The badge may still be powering up its RF field and not yet
// answering the FIFO status register, so a poll timeout here is not fatal;
// Android proceeds after the fixed 700ms settle sleep regardless.
func (writer *NFCA397Writer) wakeBadge(ctx context.Context) error {
	if err := writer.writeAndExecuteFIFO(ctx, []byte{0x11, 0x22}); err != nil {
		return err
	}
	if err := writer.pollFIFOStatus(ctx, 20); err != nil && !errors.Is(err, errFIFOPollTimeout) {
		return err
	}
	return sleepContext(ctx, 700*time.Millisecond)
}

func (writer *NFCA397Writer) writeImageBlock(ctx context.Context, packaged []byte) (int, error) {
	if len(packaged) <= fifoPayloadMax {
		if err := writer.executeFIFOAndPoll(ctx, packaged, 300); err != nil {
			return 0, err
		}
		return 1, nil
	}
	return writer.writeHighFIFO(ctx, packaged)
}

func (writer *NFCA397Writer) writeHighFIFO(ctx context.Context, payload []byte) (int, error) {
	chunk := payload
	if len(chunk) > fifoPayloadMax {
		chunk = payload[:fifoPayloadMax]
	}
	if err := writer.executeFIFOAndPoll(ctx, chunk, 200); err != nil {
		return 0, err
	}
	if len(payload) <= fifoPayloadMax {
		return 1, nil
	}
	writes, err := writer.writeHighFIFO(ctx, payload[fifoPayloadMax:])
	if err != nil {
		return 0, err
	}
	return writes + 1, nil
}

var errFIFOPollTimeout = errors.New("badge FIFO execution timed out")

func (writer *NFCA397Writer) writeAndExecuteFIFO(ctx context.Context, payload []byte) error {
	if len(payload) == 0 || len(payload) > fifoPayloadMax {
		return fmt.Errorf("FIFO payload length %d is outside 1..%d", len(payload), fifoPayloadMax)
	}

	command := make([]byte, 2+len(payload))
	command[0] = 0xAE
	command[1] = byte(len(payload) - 1)
	copy(command[2:], payload)
	response, err := writer.transceiveNative(ctx, command)
	if err != nil {
		return fmt.Errorf("write FIFO: %w", err)
	}
	if !equalBytes(response, []byte{0xAA, 0xAA}) {
		return fmt.Errorf("write FIFO returned %X, want AAAA", response)
	}

	response, err = writer.transceiveNative(ctx, []byte{0xAF, 0xFE, 0x01, 0x03})
	if err != nil {
		return fmt.Errorf("execute FIFO: %w", err)
	}
	if !equalBytes(response, []byte{0xAA, 0xAA}) {
		return fmt.Errorf("execute FIFO returned %X, want AAAA", response)
	}
	return nil
}

func (writer *NFCA397Writer) pollFIFOStatus(ctx context.Context, attempts int) error {
	for attempt := 0; attempt < attempts; attempt++ {
		if err := sleepContext(ctx, 10*time.Millisecond); err != nil {
			return err
		}
		response, err := writer.transceiveNative(ctx, []byte{0x3F, 0xA2})
		if err != nil {
			return fmt.Errorf("read FIFO status: %w", err)
		}
		if len(response) > 0 && response[0] == 0 {
			return nil
		}
	}
	return fmt.Errorf("%w after %d polls", errFIFOPollTimeout, attempts)
}

func (writer *NFCA397Writer) executeFIFOAndPoll(ctx context.Context, payload []byte, attempts int) error {
	if err := writer.writeAndExecuteFIFO(ctx, payload); err != nil {
		return err
	}
	return writer.pollFIFOStatus(ctx, attempts)
}

func (writer *NFCA397Writer) transceiveNative(ctx context.Context, command []byte) ([]byte, error) {
	if len(command) == 0 || len(command) > nfcaPassThroughMax {
		return nil, fmt.Errorf("native command length %d is outside 1..%d", len(command), nfcaPassThroughMax)
	}
	if writer.transport != nfcaTransportTransparent {
		response, err := writer.passThrough(ctx, command)
		if err == nil {
			writer.transport = nfcaTransportPassThrough
			return response, nil
		}
		if !shouldRetryTransparent(err) {
			return nil, err
		}
		writer.transport = nfcaTransportTransparent
	}
	return writer.transparentExchange(ctx, command)
}

func shouldRetryTransparent(err error) bool {
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.Status {
	case 0x6300, 0x6900, 0x6A81, 0x6D00:
		return true
	default:
		return false
	}
}

func (writer *NFCA397Writer) passThrough(ctx context.Context, command []byte) ([]byte, error) {
	apdu := make([]byte, 5+len(command))
	apdu[0] = 0xFF
	apdu[4] = byte(len(command))
	copy(apdu[5:], command)
	return transmitOK(ctx, writer.card, apdu)
}

func (writer *NFCA397Writer) transparentExchange(ctx context.Context, command []byte) ([]byte, error) {
	var rawTransmitErrs []string
	for _, candidate := range rawEscapeAPDUCandidates(command) {
		response, err := transmitOK(ctx, writer.card, candidate.payload)
		if err != nil {
			rawTransmitErrs = append(rawTransmitErrs, fmt.Sprintf("%s transmit: %v", candidate.name, err))
			if isStatus(err, 0x6700) {
				continue
			}
			break
		}
		if parsed, parseErr := parseControlResponse(response); parseErr == nil {
			return parsed, nil
		} else {
			rawTransmitErrs = append(rawTransmitErrs, fmt.Sprintf("%s parse: %v; response=%X", candidate.name, parseErr, response))
		}
	}

	commandTLV := appendBERLength([]byte{0x95}, len(command))
	commandTLV = append(commandTLV, command...)
	data := []byte{
		0x90, 0x02, 0x10, 0x00,
		0x5F, 0x46, 0x04, 0x40, 0x42, 0x0F, 0x00,
	}
	data = append(data, commandTLV...)
	var transmitErrs []string
	for _, format := range []transparentAPDUFormat{
		transparentAPDUShortWithLe,
		transparentAPDUShortNoLe,
		transparentAPDUExtendedNoLe,
		transparentAPDUExtendedWithLe,
	} {
		apdu, err := buildTransparentExchangeAPDU(data, format)
		if err != nil {
			transmitErrs = append(transmitErrs, fmt.Sprintf("%s build: %v", format, err))
			continue
		}
		response, err := transmitOK(ctx, writer.card, apdu)
		if err != nil {
			transmitErrs = append(transmitErrs, fmt.Sprintf("%s transmit: %v", format, err))
			if isStatus(err, 0x6700) {
				continue
			}
			return nil, fmt.Errorf("ACR1552 transparent exchange: %w", err)
		}
		return parseTransparentResponse(response)
	}
	var controlErrs []string
	if controller, ok := writer.card.(controlCard); ok {
		controlCodes, err := readerEscapeControlCodes(ctx, controller)
		if err == nil {
			for _, controlCode := range controlCodes {
				for _, candidate := range controlPayloadCandidates(data, commandTLV, command) {
					response, controlErr := controller.Control(ctx, controlCode, candidate.payload)
					if controlErr != nil {
						controlErrs = append(controlErrs, fmt.Sprintf("%s control(0x%08X): %v", candidate.name, controlCode, controlErr))
						continue
					}
					if parsed, parseErr := parseControlResponse(response); parseErr == nil {
						return parsed, nil
					} else {
						controlErrs = append(controlErrs, fmt.Sprintf("%s control(0x%08X) parse: %v; response=%X", candidate.name, controlCode, parseErr, response))
					}
				}
			}
		} else {
			controlErrs = append(controlErrs, fmt.Sprintf("feature discovery: %v", err))
		}
	}
	var legacyErrs []string
	if controller, ok := writer.card.(legacyControlCard); ok {
		for _, format := range []transparentAPDUFormat{
			transparentAPDUShortWithLe,
			transparentAPDUShortNoLe,
			transparentAPDUExtendedNoLe,
			transparentAPDUExtendedWithLe,
		} {
			apdu, err := buildTransparentExchangeAPDU(data, format)
			if err != nil {
				legacyErrs = append(legacyErrs, fmt.Sprintf("%s build: %v", format, err))
				continue
			}
			response, err := controller.ControlLegacy(ctx, apdu)
			if err != nil {
				legacyErrs = append(legacyErrs, fmt.Sprintf("%s legacy-control: %v", format, err))
				continue
			}
			payload, err := unwrapStatusResponse(response)
			if err != nil {
				legacyErrs = append(legacyErrs, fmt.Sprintf("%s legacy-control unwrap: %v", format, err))
				continue
			}
			return parseTransparentResponse(payload)
		}
	}
	parts := make([]string, 0, 3)
	if len(rawTransmitErrs) > 0 {
		parts = append(parts, "raw-transmit["+strings.Join(rawTransmitErrs, "; ")+"]")
	}
	if len(transmitErrs) > 0 {
		parts = append(parts, "transmit["+strings.Join(transmitErrs, "; ")+"]")
	}
	if len(controlErrs) > 0 {
		parts = append(parts, "control["+strings.Join(controlErrs, "; ")+"]")
	}
	if len(legacyErrs) > 0 {
		parts = append(parts, "legacy["+strings.Join(legacyErrs, "; ")+"]")
	}
	if len(parts) == 0 {
		return nil, errors.New("ACR1552 transparent exchange failed without diagnostics")
	}
	return nil, fmt.Errorf("ACR1552 transparent exchange failed: %s", strings.Join(parts, " | "))
}

type transparentAPDUFormat uint8

const (
	transparentAPDUShortWithLe transparentAPDUFormat = iota
	transparentAPDUShortNoLe
	transparentAPDUExtendedNoLe
	transparentAPDUExtendedWithLe
)

func (format transparentAPDUFormat) String() string {
	switch format {
	case transparentAPDUShortWithLe:
		return "short+le"
	case transparentAPDUShortNoLe:
		return "short"
	case transparentAPDUExtendedNoLe:
		return "extended"
	case transparentAPDUExtendedWithLe:
		return "extended+le"
	default:
		return fmt.Sprintf("format(%d)", format)
	}
}

func buildTransparentExchangeAPDU(data []byte, format transparentAPDUFormat) ([]byte, error) {
	switch format {
	case transparentAPDUShortWithLe:
		if len(data) > 255 {
			return nil, fmt.Errorf("transparent exchange data is %d bytes, maximum is 255 for short APDU", len(data))
		}
		apdu := make([]byte, 0, 6+len(data))
		apdu = append(apdu, 0xFF, 0xC2, 0x00, 0x01, byte(len(data)))
		apdu = append(apdu, data...)
		apdu = append(apdu, 0x00)
		return apdu, nil
	case transparentAPDUShortNoLe:
		if len(data) > 255 {
			return nil, fmt.Errorf("transparent exchange data is %d bytes, maximum is 255 for short APDU", len(data))
		}
		apdu := make([]byte, 0, 5+len(data))
		apdu = append(apdu, 0xFF, 0xC2, 0x00, 0x01, byte(len(data)))
		apdu = append(apdu, data...)
		return apdu, nil
	case transparentAPDUExtendedNoLe:
		apdu := make([]byte, 0, 7+len(data))
		apdu = append(apdu, 0xFF, 0xC2, 0x00, 0x01, 0x00, byte(len(data)>>8), byte(len(data)))
		apdu = append(apdu, data...)
		return apdu, nil
	case transparentAPDUExtendedWithLe:
		apdu := make([]byte, 0, 9+len(data))
		apdu = append(apdu, 0xFF, 0xC2, 0x00, 0x01, 0x00, byte(len(data)>>8), byte(len(data)))
		apdu = append(apdu, data...)
		apdu = append(apdu, 0x00, 0x00)
		return apdu, nil
	default:
		return nil, fmt.Errorf("unsupported transparent APDU format %d", format)
	}
}

func isStatus(err error, status uint16) bool {
	var statusErr *StatusError
	return errors.As(err, &statusErr) && statusErr.Status == status
}

func unwrapStatusResponse(response []byte) ([]byte, error) {
	if len(response) < 2 {
		return nil, fmt.Errorf("%w: response has %d bytes", errMalformedResponse, len(response))
	}
	status := uint16(response[len(response)-2])<<8 | uint16(response[len(response)-1])
	if status != 0x9000 {
		return nil, &StatusError{Status: status}
	}
	return response[:len(response)-2], nil
}

type controlPayloadCandidate struct {
	name    string
	payload []byte
}

func rawEscapeAPDUCandidates(command []byte) []controlPayloadCandidate {
	candidates := make([]controlPayloadCandidate, 0, 4)
	for _, p2 := range []byte{0x00, 0x01} {
		for _, withLe := range []bool{true, false} {
			apdu := make([]byte, 0, 5+len(command)+1)
			apdu = append(apdu, 0xFF, 0xC2, 0x00, p2, byte(len(command)))
			apdu = append(apdu, command...)
			name := fmt.Sprintf("ff-c2-p2-%02X", p2)
			if withLe {
				apdu = append(apdu, 0x00)
				name += "+le"
			} else {
				name += "-no-le"
			}
			candidates = append(candidates, controlPayloadCandidate{
				name:    name,
				payload: apdu,
			})
		}
	}
	return candidates
}

func controlPayloadCandidates(data, commandTLV, command []byte) []controlPayloadCandidate {
	candidates := make([]controlPayloadCandidate, 0, 8)
	candidates = append(candidates, controlPayloadCandidate{
		name:    "native",
		payload: append([]byte(nil), command...),
	})
	for _, format := range []transparentAPDUFormat{
		transparentAPDUShortWithLe,
		transparentAPDUShortNoLe,
		transparentAPDUExtendedNoLe,
		transparentAPDUExtendedWithLe,
	} {
		apdu, err := buildTransparentExchangeAPDU(data, format)
		if err == nil {
			candidates = append(candidates, controlPayloadCandidate{
				name:    format.String(),
				payload: apdu,
			})
		}
	}
	candidates = append(candidates,
		controlPayloadCandidate{name: "body", payload: append([]byte(nil), data...)},
		controlPayloadCandidate{name: "command-tlv", payload: append([]byte(nil), commandTLV...)},
	)
	return candidates
}

func parseControlResponse(response []byte) ([]byte, error) {
	if len(response) == 0 {
		return nil, errors.New("empty response")
	}
	if payload, err := unwrapStatusResponse(response); err == nil {
		if parsed, parseErr := parseTransparentResponse(payload); parseErr == nil {
			return parsed, nil
		}
		if len(payload) > 0 {
			return payload, nil
		}
	}
	if parsed, err := parseTransparentResponse(response); err == nil {
		return parsed, nil
	}
	return response, nil
}

func readerEscapeControlCode(ctx context.Context, controller controlCard) (uint32, error) {
	codes, err := readerEscapeControlCodes(ctx, controller)
	if err != nil {
		return 0, err
	}
	return codes[0], nil
}

func readerEscapeControlCodes(ctx context.Context, controller controlCard) ([]uint32, error) {
	response, err := controller.Control(ctx, cmIOCTLGetFeatureRequest, []byte{})
	if err != nil {
		return nil, fmt.Errorf("query PC/SC reader features: %w", err)
	}
	for offset := 0; offset+6 <= len(response); offset += 6 {
		tag := response[offset]
		length := int(response[offset+1])
		if length != 4 || offset+2+length > len(response) {
			return nil, fmt.Errorf("%w: malformed PC/SC feature TLV at offset %d; response=%X", errMalformedResponse, offset, response)
		}
		if tag == featureCCIDEscapeCommand {
			code := uint32(response[offset+2])<<24 |
				uint32(response[offset+3])<<16 |
				uint32(response[offset+4])<<8 |
				uint32(response[offset+5])
			swapped := byteSwap32(code)
			if swapped != code {
				return []uint32{code, swapped}, nil
			}
			return []uint32{code}, nil
		}
	}
	return nil, errors.New("reader does not advertise FEATURE_CCID_ESC_COMMAND")
}

func byteSwap32(value uint32) uint32 {
	return value>>24 |
		(value>>8)&0x0000FF00 |
		(value<<8)&0x00FF0000 |
		value<<24
}

func (writer *NFCA397Writer) report(completed, total int) {
	if writer.progress != nil {
		writer.progress(completed, total)
	}
}

func packageImageBlock(block []byte) []byte {
	segmentCount := (len(block) + imageSegmentMax - 1) / imageSegmentMax
	result := make([]byte, 0, len(block)+(segmentCount*4))
	for offset := 0; offset < len(block); offset += imageSegmentMax {
		end := offset + imageSegmentMax
		command := byte(0xF7)
		if end >= len(block) {
			end = len(block)
			command = 0x77
		}
		length := end - offset
		result = append(result, command, byte(offset>>8), byte(offset), byte(length))
		result = append(result, block[offset:end]...)
	}
	return result
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func appendBERLength(destination []byte, length int) []byte {
	if length < 0x80 {
		return append(destination, byte(length))
	}
	if length <= 0xFF {
		return append(destination, 0x81, byte(length))
	}
	return append(destination, 0x82, byte(length>>8), byte(length))
}

func parseTransparentResponse(response []byte) ([]byte, error) {
	if equalBytes(response, []byte{0xC0, 0x03}) {
		return nil, errors.New("ACR1552 returned a legacy transparent-exchange error; NFC-A pass-through requires reader firmware 1.05 or later")
	}
	var cardResponse []byte
	for offset := 0; offset < len(response); {
		tlvStart := offset
		tag := uint32(response[offset])
		firstTagByte := response[offset]
		offset++
		if firstTagByte&0x1F == 0x1F {
			for {
				if offset >= len(response) {
					return nil, malformedTransparentResponse(response, tlvStart, "truncated BER tag")
				}
				tagByte := response[offset]
				offset++
				tag = tag<<8 | uint32(tagByte)
				if tagByte&0x80 == 0 {
					break
				}
			}
		}
		if offset >= len(response) {
			return nil, malformedTransparentResponse(response, tlvStart, "truncated TLV")
		}
		lengthByte := response[offset]
		offset++
		length := int(lengthByte)
		switch lengthByte {
		case 0x81:
			if offset >= len(response) {
				return nil, malformedTransparentResponse(response, tlvStart, "truncated TLV length")
			}
			length = int(response[offset])
			offset++
		case 0x82:
			if offset+1 >= len(response) {
				return nil, malformedTransparentResponse(response, tlvStart, "truncated TLV length")
			}
			length = int(response[offset])<<8 | int(response[offset+1])
			offset += 2
		default:
			if lengthByte > 0x82 {
				return nil, malformedTransparentResponse(response, tlvStart, fmt.Sprintf("unsupported BER length %02X", lengthByte))
			}
		}
		if offset+length > len(response) {
			return nil, malformedTransparentResponse(response, tlvStart, "TLV exceeds response")
		}
		value := response[offset : offset+length]
		offset += length
		switch tag {
		case 0xC0:
			if len(value) == 3 && (value[1] != 0x90 || value[2] != 0x00) {
				return nil, fmt.Errorf("transparent exchange object %d failed with status %02X%02X", value[0], value[1], value[2])
			}
		case 0x96:
			if len(value) >= 1 && value[0] != 0 {
				return nil, fmt.Errorf("transparent exchange RF status is %02X", value[0])
			}
		case 0x97:
			cardResponse = append([]byte(nil), value...)
		}
	}
	if cardResponse == nil {
		return nil, malformedTransparentResponse(response, 0, "response has no card data")
	}
	return cardResponse, nil
}

func malformedTransparentResponse(response []byte, offset int, reason string) error {
	return fmt.Errorf("%w: transparent %s at offset %d; response=%X", errMalformedResponse, reason, offset, response)
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
