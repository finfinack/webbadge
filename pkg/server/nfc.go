package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"webbadge/pkg/badgeimage"
	"webbadge/pkg/nfcbadge"
	"webbadge/pkg/nfcink"
	"webbadge/pkg/pcsc"
)

const (
	maxNFCRequestBytes  = 4 << 20
	nfcCardWaitTimeout  = 30 * time.Second
	nfcWriteBudget      = 2 * time.Minute
	nfcStatusBudget     = 2 * time.Second
	nfcTagInfoBudget    = 30 * time.Second
	nfcBadgeProbeBudget = 400 * time.Millisecond
	nfcInkProbeBudget   = 1200 * time.Millisecond
)

type nfcEvent struct {
	Stage   string `json:"stage"`
	Percent int    `json:"percent"`
	Message string `json:"message"`
}

// nfcBadgeWriter waits for a badge on an NFC reader and writes an image to
// it, reporting stage/percent/message progress as it goes. It exists so the
// HTTP/SSE plumbing in writeNFC can be tested without real PC/SC hardware.
type nfcBadgeWriter interface {
	Write(ctx context.Context, img image.Image, profile badgeimage.Profile, report func(stage string, percent int, message string)) error
}

// pcscNFCWriter writes either supported badge profile through a local
// ACR1552 PC/SC reader, dispatching to the matching protocol package.
type pcscNFCWriter struct{}

func (pcscNFCWriter) Write(ctx context.Context, img image.Image, profile badgeimage.Profile, report func(stage string, percent int, message string)) error {
	report("connecting", 0, "Opening PC/SC session…")
	session, err := pcsc.Establish()
	if err != nil {
		return fmt.Errorf("establish PC/SC session: %w", err)
	}
	defer session.Close()

	reader, err := session.SelectReader("")
	if err != nil {
		return fmt.Errorf("select PC/SC reader: %w", err)
	}

	report("waiting", 0, fmt.Sprintf("Waiting for badge on %s…", reader))
	waitCtx, cancel := context.WithTimeout(ctx, nfcCardWaitTimeout)
	card, err := session.WaitForCard(waitCtx, reader, 200*time.Millisecond)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for badge: %w", err)
	}
	defer card.Close()

	report("writing", 0, "Writing image to badge…")
	progress := func(completed, total int) {
		percent := 100
		if total > 0 {
			percent = completed * 100 / total
		}
		report("writing", percent, fmt.Sprintf("Writing to badge… %d%%", percent))
	}

	switch profile.ID {
	case badgeimage.Hybrid397.ID:
		writer := nfcbadge.NewNFCA397Writer(card, progress)
		if err := writer.Write(ctx, img); err != nil {
			return fmt.Errorf("write badge: %w", err)
		}
		return nil
	case badgeimage.NfcInk240x416.ID:
		return writeNfcInk(ctx, card, img, progress)
	default:
		return fmt.Errorf("unsupported badge profile %q", profile.ID)
	}
}

// writeNfcInk reads the badge's own reported configuration (geometry,
// colour wiring), encodes img to match it, uploads it via the
// LZO-compressed D3 path, and triggers the screen refresh.
func writeNfcInk(ctx context.Context, card *pcsc.Card, img image.Image, progress func(completed, total int)) error {
	device := nfcink.NewNfcInkDevice(card, progress)
	cfg, err := device.ReadConfig(ctx)
	if err != nil {
		return fmt.Errorf("read nfcink config: %w", err)
	}
	slog.Debug("nfcink write starting",
		"reader", card.Reader(),
		"atr", fmt.Sprintf("%X", card.ATR()),
		"width", cfg.ScreenWidth,
		"height", cfg.ScreenHeight,
		"colors", cfg.ColorCount,
		"picture_capacity", cfg.PictureCapacity,
		"palette", cfg.ColorDic)
	encoded := nfcink.EncodeImage(img, cfg)
	slog.Debug("nfcink image encoded",
		"bytes", len(encoded),
		"preview", summarizeNFCBytes(encoded, 16))
	if err := device.WriteImageD3(ctx, encoded, 0); err != nil {
		return fmt.Errorf("write nfcink image: %w", err)
	}
	if err := device.Refresh(ctx, 0); err != nil {
		return fmt.Errorf("refresh nfcink badge: %w", err)
	}
	return nil
}

func summarizeNFCBytes(data []byte, preview int) string {
	if len(data) == 0 {
		return "len=0"
	}
	if preview <= 0 {
		preview = 1
	}
	if len(data) <= preview {
		return fmt.Sprintf("len=%d hex=%X", len(data), data)
	}
	return fmt.Sprintf("len=%d head=%X tail=%X", len(data), data[:preview], data[len(data)-preview:])
}

// nfcReaderStatus is what a liveness probe found on the local NFC reader.
type nfcReaderStatus struct {
	Reader       string
	BadgePresent bool
}

type nfcTagInfo struct {
	Reader          string   `json:"reader"`
	ATR             string   `json:"atr"`
	UID             string   `json:"uid,omitempty"`
	ATS             string   `json:"ats,omitempty"`
	Content         string   `json:"content,omitempty"`
	ContentHex      string   `json:"contentHex,omitempty"`
	ContentLines    []string `json:"contentLines,omitempty"`
	ContentError    string   `json:"contentError,omitempty"`
	ProbeLines      []string `json:"probeLines,omitempty"`
	NDEFRecordType  string   `json:"ndefRecordType,omitempty"`
	NDEFRecordValue string   `json:"ndefRecordValue,omitempty"`
	NDEFSupported   bool     `json:"ndefSupported"`
	NDEFWritable    bool     `json:"ndefWritable"`
	NDEFCapacity    int      `json:"ndefCapacity,omitempty"`
}

// nfcReaderProbe checks whether a local NFC reader (and optionally a badge
// on it) is currently reachable, without performing a write. It exists so
// the status handler can be tested without real PC/SC hardware.
type nfcReaderProbe interface {
	Probe(ctx context.Context, profile badgeimage.Profile) (nfcReaderStatus, error)
}

type nfcTagInfoReader interface {
	ReadInfo(ctx context.Context) (nfcTagInfo, error)
}

type nfcTagNDEFWriter interface {
	WriteNDEF(ctx context.Context, recordType, value string) error
}

// pcscReaderProbe is the real ACR1552/PC/SC liveness check.
type pcscReaderProbe struct{}

type pcscNFCTagInfoReader struct{}

type ndefCapability struct {
	FileID   []byte
	MaxSize  int
	Writable bool
}

func (pcscReaderProbe) Probe(ctx context.Context, profile badgeimage.Profile) (nfcReaderStatus, error) {
	session, err := pcsc.Establish()
	if err != nil {
		return nfcReaderStatus{}, fmt.Errorf("establish PC/SC session: %w", err)
	}
	defer session.Close()

	reader, err := session.SelectReader("")
	if err != nil {
		return nfcReaderStatus{}, fmt.Errorf("select PC/SC reader: %w", err)
	}

	// A badge not answering within this short window just means there's no
	// badge on the reader right now, which isn't an error worth surfacing.
	probeBudget := nfcBadgeProbeBudget
	if profile.ID == badgeimage.NfcInk240x416.ID {
		probeBudget = nfcInkProbeBudget
	}
	badgeCtx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	card, err := session.WaitForCard(badgeCtx, reader, 100*time.Millisecond)
	if err != nil {
		return nfcReaderStatus{Reader: reader}, nil
	}
	defer card.Close()

	present, err := probeBadgeProfile(badgeCtx, card, profile)
	if err != nil {
		return nfcReaderStatus{Reader: reader}, nil
	}
	return nfcReaderStatus{Reader: reader, BadgePresent: present}, nil
}

func (pcscNFCTagInfoReader) ReadInfo(ctx context.Context) (nfcTagInfo, error) {
	session, err := pcsc.Establish()
	if err != nil {
		return nfcTagInfo{}, fmt.Errorf("establish PC/SC session: %w", err)
	}
	defer session.Close()

	reader, err := session.SelectReader("")
	if err != nil {
		return nfcTagInfo{}, fmt.Errorf("select PC/SC reader: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, nfcCardWaitTimeout)
	card, err := session.WaitForCard(waitCtx, reader, 200*time.Millisecond)
	cancel()
	if err != nil {
		return nfcTagInfo{}, fmt.Errorf("wait for tag: %w", err)
	}
	defer card.Close()

	info := nfcTagInfo{
		Reader: reader,
		ATR:    fmt.Sprintf("%X", card.ATR()),
	}

	if uid, err := readTagDataAPDU(ctx, card, []byte{0xFF, 0xCA, 0x00, 0x00, 0x00}, "UID"); err == nil {
		info.UID = fmt.Sprintf("%X", uid)
	} else {
		return nfcTagInfo{}, err
	}
	if ats, err := readTagDataAPDU(ctx, card, []byte{0xFF, 0xCA, 0x01, 0x00, 0x00}, "ATS"); err == nil {
		info.ATS = fmt.Sprintf("%X", ats)
	}
	if capability, err := readNDEFCapability(ctx, card); err == nil {
		info.NDEFSupported = true
		info.NDEFWritable = capability.Writable
		info.NDEFCapacity = capability.MaxSize
		if payload, err := readSelectedNDEFContent(ctx, card, capability); err == nil {
			info.ContentHex = fmt.Sprintf("%X", payload)
			info.Content, info.ContentLines = summarizeNDEF(payload)
			info.NDEFRecordType, info.NDEFRecordValue = extractEditableNDEFRecord(payload)
		} else {
			info.ContentError = err.Error()
		}
	} else if payload, err := readNDEFContent(ctx, card); err == nil {
		info.NDEFSupported = true
		info.ContentHex = fmt.Sprintf("%X", payload)
		info.Content, info.ContentLines = summarizeNDEF(payload)
		info.NDEFRecordType, info.NDEFRecordValue = extractEditableNDEFRecord(payload)
	} else if payload, writable, capacity, err := readType2NDEFContent(ctx, card); err == nil {
		info.NDEFSupported = true
		info.NDEFWritable = writable
		info.NDEFCapacity = capacity
		info.ContentHex = fmt.Sprintf("%X", payload)
		info.Content, info.ContentLines = summarizeNDEF(payload)
		info.NDEFRecordType, info.NDEFRecordValue = extractEditableNDEFRecord(payload)
	} else {
		info.ContentError = err.Error()
	}
	info.ProbeLines = probeTagContent(ctx, card)
	return info, nil
}

func (pcscNFCTagInfoReader) WriteNDEF(ctx context.Context, recordType, value string) error {
	session, err := pcsc.Establish()
	if err != nil {
		return fmt.Errorf("establish PC/SC session: %w", err)
	}
	defer session.Close()

	reader, err := session.SelectReader("")
	if err != nil {
		return fmt.Errorf("select PC/SC reader: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, nfcCardWaitTimeout)
	card, err := session.WaitForCard(waitCtx, reader, 200*time.Millisecond)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for tag: %w", err)
	}
	defer card.Close()

	capability, err := readNDEFCapability(ctx, card)
	if err != nil {
		return err
	}
	if !capability.Writable {
		return errors.New("tag exposes NDEF but is not writable")
	}
	payload, err := encodeNDEFMessage(recordType, value)
	if err != nil {
		return err
	}
	return writeNDEFFile(ctx, card, capability, payload)
}

// writeNDEFFile writes a raw NDEF message to the already-discovered NDEF file,
// clearing the length header first so a reader never observes a mix of old
// and new payload bytes.
func writeNDEFFile(ctx context.Context, card *pcsc.Card, capability ndefCapability, payload []byte) error {
	if len(payload)+2 > capability.MaxSize {
		return fmt.Errorf("NDEF payload is %d bytes, maximum is %d", len(payload)+2, capability.MaxSize)
	}
	if err := selectFile(ctx, card, capability.FileID); err != nil {
		return fmt.Errorf("select NDEF file: %w", err)
	}
	if err := updateBinary(ctx, card, 0, []byte{0x00, 0x00}); err != nil {
		return fmt.Errorf("clear NDEF header: %w", err)
	}
	if err := updateBinary(ctx, card, 2, payload); err != nil {
		return fmt.Errorf("write NDEF payload: %w", err)
	}
	header := []byte{byte(len(payload) >> 8), byte(len(payload))}
	if err := updateBinary(ctx, card, 0, header); err != nil {
		return fmt.Errorf("write NDEF header: %w", err)
	}
	return nil
}

func readTagDataAPDU(ctx context.Context, card *pcsc.Card, command []byte, field string) ([]byte, error) {
	response, err := card.Transmit(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("read tag %s: %w", strings.ToLower(field), err)
	}
	if len(response) < 2 {
		return nil, fmt.Errorf("read tag %s: short response %X", strings.ToLower(field), response)
	}
	sw := response[len(response)-2:]
	if sw[0] != 0x90 || sw[1] != 0x00 {
		return nil, fmt.Errorf("read tag %s: card returned status %X", strings.ToLower(field), sw)
	}
	return append([]byte(nil), response[:len(response)-2]...), nil
}

func readNDEFContent(ctx context.Context, card *pcsc.Card) ([]byte, error) {
	capability, err := readNDEFCapability(ctx, card)
	if err != nil {
		return nil, err
	}
	return readSelectedNDEFContent(ctx, card, capability)
}

func readNDEFCapability(ctx context.Context, card *pcsc.Card) (ndefCapability, error) {
	if err := selectByName(ctx, card, []byte{0xD2, 0x76, 0x00, 0x00, 0x85, 0x01, 0x01}); err != nil {
		return ndefCapability{}, fmt.Errorf("select NDEF application: %w", err)
	}
	ccFile, err := selectAndReadFile(ctx, card, []byte{0xE1, 0x03}, 15)
	if err != nil {
		return ndefCapability{}, fmt.Errorf("read capability container: %w", err)
	}
	return parseCapabilityContainer(ccFile)
}

func readSelectedNDEFContent(ctx context.Context, card *pcsc.Card, capability ndefCapability) ([]byte, error) {
	header, err := selectAndReadFile(ctx, card, capability.FileID, 2)
	if err != nil {
		return nil, fmt.Errorf("read NDEF header: %w", err)
	}
	if len(header) != 2 {
		return nil, fmt.Errorf("read NDEF header: short response %X", header)
	}
	length := int(binary.BigEndian.Uint16(header))
	if length == 0 {
		return nil, errors.New("NDEF file is empty")
	}
	return readBinary(ctx, card, 2, length)
}

func readType2NDEFContent(ctx context.Context, card *pcsc.Card) ([]byte, bool, int, error) {
	// Read enough pages to cover the capability container and the first TLVs.
	pages, err := transmitData(ctx, card, []byte{0xFF, 0xB0, 0x00, 0x03, 0x30}, "read type 2 pages")
	if err != nil {
		return nil, false, 0, fmt.Errorf("read type 2 pages: %w", err)
	}
	if len(pages) < 16 {
		return nil, false, 0, fmt.Errorf("type 2 page read too short: %X", pages)
	}

	cc := pages[:4]
	if cc[0] != 0xE1 || (cc[1]&0xF0) != 0x10 {
		return nil, false, 0, fmt.Errorf("type 2 capability container not found: %X", cc)
	}
	capacity := int(cc[2]) * 8
	writable := (cc[3] & 0x0F) == 0x00

	userData := pages[4:]
	payload, err := parseType2TLV(userData)
	if err != nil {
		return nil, writable, capacity, err
	}
	return payload, writable, capacity, nil
}

func parseType2TLV(data []byte) ([]byte, error) {
	for index := 0; index < len(data); {
		tag := data[index]
		index++
		switch tag {
		case 0x00:
			continue
		case 0xFE:
			return nil, errors.New("type 2 terminator reached before NDEF TLV")
		case 0x03:
			if index >= len(data) {
				return nil, errors.New("truncated type 2 NDEF TLV length")
			}
			length := int(data[index])
			index++
			if length == 0xFF {
				if index+2 > len(data) {
					return nil, errors.New("truncated extended type 2 NDEF TLV length")
				}
				length = int(binary.BigEndian.Uint16(data[index : index+2]))
				index += 2
			}
			if index+length > len(data) {
				return nil, errors.New("truncated type 2 NDEF TLV payload")
			}
			return append([]byte(nil), data[index:index+length]...), nil
		default:
			if index >= len(data) {
				return nil, fmt.Errorf("truncated type 2 TLV length for tag %02X", tag)
			}
			length := int(data[index])
			index++
			if length == 0xFF {
				if index+2 > len(data) {
					return nil, fmt.Errorf("truncated extended type 2 TLV length for tag %02X", tag)
				}
				length = int(binary.BigEndian.Uint16(data[index : index+2]))
				index += 2
			}
			if index+length > len(data) {
				return nil, fmt.Errorf("truncated type 2 TLV value for tag %02X", tag)
			}
			index += length
		}
	}
	return nil, errors.New("no type 2 NDEF TLV found")
}

func selectByName(ctx context.Context, card *pcsc.Card, aid []byte) error {
	command := make([]byte, 0, 6+len(aid))
	command = append(command, 0x00, 0xA4, 0x04, 0x00, byte(len(aid)))
	command = append(command, aid...)
	command = append(command, 0x00)
	return transmitOK(ctx, card, command, "select by name")
}

func selectAndReadFile(ctx context.Context, card *pcsc.Card, fileID []byte, length int) ([]byte, error) {
	if err := selectFile(ctx, card, fileID); err != nil {
		return nil, err
	}
	return readBinary(ctx, card, 0, length)
}

func readBinary(ctx context.Context, card *pcsc.Card, offset, length int) ([]byte, error) {
	if length < 0 || offset < 0 {
		return nil, errors.New("negative read range")
	}
	data := make([]byte, 0, length)
	for remaining := length; remaining > 0; {
		chunk := remaining
		if chunk > 0xFF {
			chunk = 0xFF
		}
		command := []byte{0x00, 0xB0, byte(offset >> 8), byte(offset), byte(chunk)}
		response, err := transmitData(ctx, card, command, "read binary")
		if err != nil {
			return nil, err
		}
		data = append(data, response...)
		offset += len(response)
		remaining -= len(response)
		if len(response) == 0 {
			return nil, errors.New("read binary returned no data")
		}
	}
	return data, nil
}

func parseCapabilityContainer(data []byte) (ndefCapability, error) {
	if len(data) < 15 {
		return ndefCapability{}, fmt.Errorf("capability container is too short: %X", data)
	}
	if binary.BigEndian.Uint16(data[:2]) < 15 {
		return ndefCapability{}, fmt.Errorf("capability container length is too small: %X", data[:2])
	}
	if data[7] != 0x04 || data[8] != 0x06 {
		return ndefCapability{}, fmt.Errorf("capability container has no NDEF file control TLV: %X", data)
	}
	return ndefCapability{
		FileID:   []byte{data[9], data[10]},
		MaxSize:  int(binary.BigEndian.Uint16(data[11:13])),
		Writable: data[14] != 0xFF,
	}, nil
}

func transmitOK(ctx context.Context, card *pcsc.Card, command []byte, action string) error {
	_, err := transmitData(ctx, card, command, action)
	return err
}

func selectFile(ctx context.Context, card *pcsc.Card, fileID []byte) error {
	if len(fileID) != 2 {
		return fmt.Errorf("invalid file id length %d", len(fileID))
	}
	return transmitOK(ctx, card, []byte{0x00, 0xA4, 0x00, 0x0C, 0x02, fileID[0], fileID[1]}, "select file")
}

func updateBinary(ctx context.Context, card *pcsc.Card, offset int, data []byte) error {
	for len(data) > 0 {
		chunk := data
		if len(chunk) > 0xFF {
			chunk = chunk[:0xFF]
		}
		command := make([]byte, 0, 5+len(chunk))
		command = append(command, 0x00, 0xD6, byte(offset>>8), byte(offset), byte(len(chunk)))
		command = append(command, chunk...)
		if err := transmitOK(ctx, card, command, "update binary"); err != nil {
			return err
		}
		offset += len(chunk)
		data = data[len(chunk):]
	}
	return nil
}

func transmitData(ctx context.Context, card *pcsc.Card, command []byte, action string) ([]byte, error) {
	response, err := card.Transmit(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if len(response) < 2 {
		return nil, fmt.Errorf("%s: short response %X", action, response)
	}
	sw := response[len(response)-2:]
	if sw[0] != 0x90 || sw[1] != 0x00 {
		return nil, fmt.Errorf("%s: card returned status %X", action, sw)
	}
	return append([]byte(nil), response[:len(response)-2]...), nil
}

func summarizeNDEF(data []byte) (string, []string) {
	records, err := parseNDEFRecords(data)
	if err != nil {
		return "", []string{fmt.Sprintf("Could not parse NDEF records: %v", err)}
	}
	lines := make([]string, 0, len(records))
	for _, record := range records {
		lines = append(lines, record)
	}
	return strings.Join(lines, "\n"), lines
}

func extractEditableNDEFRecord(data []byte) (string, string) {
	typeField, payload, err := firstNDEFRecord(data)
	if err != nil {
		return "", ""
	}
	switch string(typeField) {
	case "T":
		if len(payload) == 0 {
			return "", ""
		}
		languageLength := int(payload[0] & 0x3F)
		if 1+languageLength > len(payload) {
			return "", ""
		}
		return "text", string(payload[1+languageLength:])
	case "U":
		if len(payload) == 0 {
			return "", ""
		}
		prefix := ndefURIPrefix(payload[0])
		value := prefix + string(payload[1:])
		if strings.HasPrefix(value, "mailto:") {
			return "email", strings.TrimPrefix(value, "mailto:")
		}
		return "url", value
	default:
		return "", ""
	}
}

func firstNDEFRecord(data []byte) ([]byte, []byte, error) {
	if len(data) == 0 {
		return nil, nil, errors.New("empty NDEF message")
	}
	offset := 0
	header := data[offset]
	offset++
	if offset+2 > len(data) {
		return nil, nil, errors.New("truncated NDEF record header")
	}
	shortRecord := header&0x10 != 0
	idLengthPresent := header&0x08 != 0
	typeLength := int(data[offset])
	offset++
	var payloadLength int
	if shortRecord {
		payloadLength = int(data[offset])
		offset++
	} else {
		if offset+4 > len(data) {
			return nil, nil, errors.New("truncated NDEF payload length")
		}
		payloadLength = int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
	}
	idLength := 0
	if idLengthPresent {
		if offset >= len(data) {
			return nil, nil, errors.New("truncated NDEF id length")
		}
		idLength = int(data[offset])
		offset++
	}
	end := offset + typeLength + idLength + payloadLength
	if end > len(data) {
		return nil, nil, errors.New("truncated NDEF payload")
	}
	typeField := append([]byte(nil), data[offset:offset+typeLength]...)
	offset += typeLength + idLength
	payload := append([]byte(nil), data[offset:offset+payloadLength]...)
	return typeField, payload, nil
}

func probeTagContent(ctx context.Context, card *pcsc.Card) []string {
	type probe struct {
		name string
		run  func(context.Context, *pcsc.Card) string
	}
	probes := []probe{
		{
			name: "Type 4 NDEF application select",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0x00, 0xA4, 0x04, 0x00, 0x07, 0xD2, 0x76, 0x00, 0x00, 0x85, 0x01, 0x01, 0x00})
			},
		},
		{
			name: "Storage-card read block/page 0",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0xFF, 0xB0, 0x00, 0x00, 0x10})
			},
		},
		{
			name: "Storage-card read block/page 4",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0xFF, 0xB0, 0x00, 0x04, 0x10})
			},
		},
		{
			name: "Storage-card read block/page 8",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0xFF, 0xB0, 0x00, 0x08, 0x10})
			},
		},
		{
			name: "ISO select MF 3F00",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0x3F, 0x00})
			},
		},
		{
			name: "ISO select EF.DIR 2F00",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return chainedProbe(ctx, card,
					[]byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0x3F, 0x00},
					[]byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0x2F, 0x00},
				)
			},
		},
		{
			name: "ISO read EF.DIR first bytes",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return chainedProbe(ctx, card,
					[]byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0x3F, 0x00},
					[]byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0x2F, 0x00},
					[]byte{0x00, 0xB0, 0x00, 0x00, 0x20},
				)
			},
		},
		{
			name: "Direct select file E103",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0xE1, 0x03})
			},
		},
		{
			name: "Direct select file E104",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0x00, 0xA4, 0x00, 0x0C, 0x02, 0xE1, 0x04})
			},
		},
		{
			name: "Select PPSE 2PAY.SYS.DDF01",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0x00, 0xA4, 0x04, 0x00, 0x0E, 0x32, 0x50, 0x41, 0x59, 0x2E, 0x53, 0x59, 0x53, 0x2E, 0x44, 0x44, 0x46, 0x30, 0x31, 0x00})
			},
		},
		{
			name: "Select PSE 1PAY.SYS.DDF01",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0x00, 0xA4, 0x04, 0x00, 0x0E, 0x31, 0x50, 0x41, 0x59, 0x2E, 0x53, 0x59, 0x53, 0x2E, 0x44, 0x44, 0x46, 0x30, 0x31, 0x00})
			},
		},
		{
			name: "ISO GET DATA 00FF",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return singleProbe(ctx, card, []byte{0x00, 0xCA, 0x00, 0xFF, 0x00})
			},
		},
		{
			name: "MIFARE Classic auth/read block 4 with default Key A",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return mifareClassicProbe(ctx, card, 4, 0x60, 0x00, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
			},
		},
		{
			name: "MIFARE Classic auth/read block 4 with NFC Forum Key A",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return mifareClassicProbe(ctx, card, 4, 0x60, 0x00, []byte{0xD3, 0xF7, 0xD3, 0xF7, 0xD3, 0xF7})
			},
		},
		{
			name: "MIFARE Classic auth/read block 4 with default Key B",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return mifareClassicProbe(ctx, card, 4, 0x61, 0x00, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
			},
		},
		{
			name: "MIFARE Classic auth/read block 1 with default Key A",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return mifareClassicProbe(ctx, card, 1, 0x60, 0x00, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
			},
		},
		{
			name: "MIFARE Classic dump sectors 0-3 with default Key A",
			run: func(ctx context.Context, card *pcsc.Card) string {
				return mifareClassicSectorDump(ctx, card, 0, 3, 0x00, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
			},
		},
	}

	lines := make([]string, 0, len(probes))
	for _, probe := range probes {
		lines = append(lines, probe.name+": "+probe.run(ctx, card))
	}
	return lines
}

func singleProbe(ctx context.Context, card *pcsc.Card, command []byte) string {
	response, err := card.Transmit(ctx, command)
	return describeProbeResponse(response, err)
}

func chainedProbe(ctx context.Context, card *pcsc.Card, commands ...[]byte) string {
	for index, command := range commands {
		response, err := card.Transmit(ctx, command)
		if err != nil {
			return fmt.Sprintf("step %d error %v", index+1, err)
		}
		outcome, ok := successfulProbeResponse(response)
		if !ok {
			return fmt.Sprintf("step %d %s", index+1, outcome)
		}
		if index == len(commands)-1 {
			return outcome
		}
	}
	return "no commands"
}

func describeProbeResponse(response []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	outcome, _ := successfulProbeResponse(response)
	return outcome
}

func mifareClassicProbe(ctx context.Context, card *pcsc.Card, block byte, keyType byte, keyNumber byte, key []byte) string {
	if len(key) != 6 {
		return "invalid key length"
	}
	loadKey := append([]byte{0xFF, 0x82, 0x00, keyNumber, byte(len(key))}, key...)
	response, err := card.Transmit(ctx, loadKey)
	if err != nil {
		return fmt.Sprintf("load key error %v", err)
	}
	outcome, ok := successfulProbeResponse(response)
	if !ok {
		return "load key " + outcome
	}

	auth := []byte{0xFF, 0x86, 0x00, 0x00, 0x05, 0x01, 0x00, block, keyType, keyNumber}
	response, err = card.Transmit(ctx, auth)
	if err != nil {
		return fmt.Sprintf("auth error %v", err)
	}
	outcome, ok = successfulProbeResponse(response)
	if !ok {
		return "auth " + outcome
	}

	response, err = card.Transmit(ctx, []byte{0xFF, 0xB0, 0x00, block, 0x10})
	if err != nil {
		return fmt.Sprintf("read error %v", err)
	}
	outcome, _ = successfulProbeResponse(response)
	return "auth ok; " + outcome
}

func mifareClassicSectorDump(ctx context.Context, card *pcsc.Card, startSector, endSector int, keyNumber byte, key []byte) string {
	if startSector < 0 || endSector < startSector {
		return "invalid sector range"
	}
	loadKey := append([]byte{0xFF, 0x82, 0x00, keyNumber, byte(len(key))}, key...)
	response, err := card.Transmit(ctx, loadKey)
	if err != nil {
		return fmt.Sprintf("load key error %v", err)
	}
	outcome, ok := successfulProbeResponse(response)
	if !ok {
		return "load key " + outcome
	}

	lines := make([]string, 0, (endSector-startSector+1)*4)
	for sector := startSector; sector <= endSector; sector++ {
		firstBlock := sector * 4
		auth := []byte{0xFF, 0x86, 0x00, 0x00, 0x05, 0x01, 0x00, byte(firstBlock), 0x60, keyNumber}
		response, err := card.Transmit(ctx, auth)
		if err != nil {
			lines = append(lines, fmt.Sprintf("sector %d auth error %v", sector, err))
			continue
		}
		outcome, ok = successfulProbeResponse(response)
		if !ok {
			lines = append(lines, fmt.Sprintf("sector %d auth %s", sector, outcome))
			continue
		}
		for block := 0; block < 4; block++ {
			blockNumber := byte(firstBlock + block)
			response, err = card.Transmit(ctx, []byte{0xFF, 0xB0, 0x00, blockNumber, 0x10})
			if err != nil {
				lines = append(lines, fmt.Sprintf("block %d error %v", blockNumber, err))
				continue
			}
			outcome, ok = successfulProbeResponse(response)
			if !ok {
				lines = append(lines, fmt.Sprintf("block %d %s", blockNumber, outcome))
				continue
			}
			lines = append(lines, fmt.Sprintf("block %d %s", blockNumber, outcome))
		}
	}
	return strings.Join(lines, " | ")
}

func successfulProbeResponse(response []byte) (string, bool) {
	if len(response) < 2 {
		return fmt.Sprintf("short response %X", response), false
	}
	sw := response[len(response)-2:]
	data := response[:len(response)-2]
	if sw[0] != 0x90 || sw[1] != 0x00 {
		return fmt.Sprintf("status %X", sw), false
	}
	return fmt.Sprintf("ok data=%s", summarizeProbeData(data)), true
}

func summarizeProbeData(data []byte) string {
	if len(data) == 0 {
		return "empty"
	}
	if isMostlyPrintable(data) {
		return fmt.Sprintf("%q", string(data))
	}
	if len(data) <= 16 {
		return fmt.Sprintf("%X", data)
	}
	return fmt.Sprintf("%X…%X", data[:8], data[len(data)-8:])
}

func parseNDEFRecords(data []byte) ([]string, error) {
	var records []string
	for offset := 0; offset < len(data); {
		header := data[offset]
		offset++
		if offset+2 > len(data) {
			return nil, errors.New("truncated NDEF record header")
		}
		shortRecord := header&0x10 != 0
		idLengthPresent := header&0x08 != 0
		tnf := header & 0x07
		typeLength := int(data[offset])
		offset++

		var payloadLength int
		if shortRecord {
			payloadLength = int(data[offset])
			offset++
		} else {
			if offset+4 > len(data) {
				return nil, errors.New("truncated NDEF payload length")
			}
			payloadLength = int(binary.BigEndian.Uint32(data[offset : offset+4]))
			offset += 4
		}

		idLength := 0
		if idLengthPresent {
			if offset >= len(data) {
				return nil, errors.New("truncated NDEF id length")
			}
			idLength = int(data[offset])
			offset++
		}

		end := offset + typeLength + idLength + payloadLength
		if end > len(data) {
			return nil, errors.New("truncated NDEF payload")
		}
		typeField := data[offset : offset+typeLength]
		offset += typeLength + idLength
		payload := data[offset : offset+payloadLength]
		offset += payloadLength

		records = append(records, describeNDEFRecord(tnf, typeField, payload))
		if header&0x40 != 0 {
			break
		}
	}
	if len(records) == 0 {
		return nil, errors.New("no NDEF records found")
	}
	return records, nil
}

func describeNDEFRecord(tnf byte, typeField, payload []byte) string {
	if tnf == 0x01 && string(typeField) == "T" && len(payload) > 0 {
		languageLength := int(payload[0] & 0x3F)
		if 1+languageLength <= len(payload) {
			return fmt.Sprintf("Text: %s", string(payload[1+languageLength:]))
		}
	}
	if tnf == 0x01 && string(typeField) == "U" && len(payload) > 0 {
		prefix := ndefURIPrefix(payload[0])
		return fmt.Sprintf("URI: %s%s", prefix, string(payload[1:]))
	}
	if tnf == 0x02 {
		return fmt.Sprintf("MIME %q: %s", string(typeField), summarizePayload(payload))
	}
	return fmt.Sprintf("TNF %d type %q: %s", tnf, string(typeField), summarizePayload(payload))
}

func summarizePayload(payload []byte) string {
	if len(payload) == 0 {
		return "empty"
	}
	if isMostlyPrintable(payload) {
		return string(payload)
	}
	return fmt.Sprintf("hex=%X", payload)
}

func isMostlyPrintable(payload []byte) bool {
	printable := 0
	for _, value := range payload {
		if value >= 0x20 && value <= 0x7E {
			printable++
		}
	}
	return printable*100 >= len(payload)*85
}

func ndefURIPrefix(code byte) string {
	switch code {
	case 0x00:
		return ""
	case 0x01:
		return "http://www."
	case 0x02:
		return "https://www."
	case 0x03:
		return "http://"
	case 0x04:
		return "https://"
	default:
		return ""
	}
}

func encodeNDEFMessage(recordType, value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("NDEF value must not be empty")
	}
	switch recordType {
	case "text":
		payload := append([]byte{0x02, 'e', 'n'}, []byte(value)...)
		return encodeShortNDEFRecord(0x01, []byte("T"), payload), nil
	case "url":
		prefix, remainder := compressURI(value)
		return encodeShortNDEFRecord(0x01, []byte("U"), append([]byte{prefix}, []byte(remainder)...)), nil
	case "email":
		prefix, remainder := compressURI("mailto:" + value)
		return encodeShortNDEFRecord(0x01, []byte("U"), append([]byte{prefix}, []byte(remainder)...)), nil
	default:
		return nil, fmt.Errorf("unsupported NDEF record type %q", recordType)
	}
}

func encodeShortNDEFRecord(tnf byte, recordType, payload []byte) []byte {
	record := make([]byte, 0, 3+len(recordType)+len(payload))
	record = append(record, 0xD0|tnf)
	record = append(record, byte(len(recordType)), byte(len(payload)))
	record = append(record, recordType...)
	record = append(record, payload...)
	return record
}

func compressURI(value string) (byte, string) {
	prefixes := []struct {
		code   byte
		prefix string
	}{
		{0x02, "https://www."},
		{0x01, "http://www."},
		{0x04, "https://"},
		{0x03, "http://"},
		{0x06, "mailto:"},
	}
	for _, candidate := range prefixes {
		if strings.HasPrefix(value, candidate.prefix) {
			return candidate.code, strings.TrimPrefix(value, candidate.prefix)
		}
	}
	return 0x00, value
}

func probeBadgeProfile(ctx context.Context, card *pcsc.Card, profile badgeimage.Profile) (bool, error) {
	switch profile.ID {
	case "", badgeimage.Hybrid397.ID:
		return true, nil
	case badgeimage.NfcInk240x416.ID:
		_, err := nfcink.NewNfcInkDevice(card, nil).ReadConfig(ctx)
		if err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported badge profile %q", profile.ID)
	}
}

type nfcStatusResponse struct {
	Available    bool   `json:"available"`
	Reader       string `json:"reader,omitempty"`
	BadgePresent bool   `json:"badgePresent"`
	Error        string `json:"error,omitempty"`
}

func (service *api) nfcStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), nfcStatusBudget)
	defer cancel()

	profile := badgeimage.Hybrid397
	if profileID := request.URL.Query().Get("profile"); profileID != "" {
		var ok bool
		profile, ok = badgeimage.ProfileByID(profileID)
		if !ok {
			writeError(writer, http.StatusBadRequest, "unknown badge profile")
			return
		}
	}

	status, err := service.nfcProbe.Probe(ctx, profile)
	if err != nil {
		writeJSON(writer, http.StatusOK, nfcStatusResponse{Available: false, Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, nfcStatusResponse{
		Available:    true,
		Reader:       status.Reader,
		BadgePresent: status.BadgePresent,
	})
}

func (service *api) writeNFC(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxNFCRequestBytes)
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(writer, http.StatusRequestEntityTooLarge, "image is too large")
			return
		}
		writeError(writer, http.StatusBadRequest, "invalid multipart request")
		return
	}
	if request.MultipartForm != nil {
		defer func() {
			if err := request.MultipartForm.RemoveAll(); err != nil {
				slog.Warn("remove multipart temporary files", "error", err)
			}
		}()
	}

	profile, ok := badgeimage.ProfileByID(request.FormValue("profile"))
	if !ok {
		writeError(writer, http.StatusBadRequest, "unknown badge profile")
		return
	}

	file, _, err := request.FormFile("image")
	if err != nil {
		writeError(writer, http.StatusBadRequest, "one image file is required")
		return
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			slog.Warn("close uploaded image", "error", closeErr)
		}
	}()

	img, err := png.Decode(file)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "image must be a valid PNG")
		return
	}
	bounds := img.Bounds()
	if bounds.Dx() != profile.Width || bounds.Dy() != profile.Height {
		writeError(writer, http.StatusBadRequest,
			fmt.Sprintf("image must be exactly %dx%d pixels", profile.Width, profile.Height))
		return
	}

	select {
	case service.nfcSlots <- struct{}{}:
		defer func() { <-service.nfcSlots }()
	case <-request.Context().Done():
		return
	}

	// Extend only this response's write deadline rather than the server-wide
	// WriteTimeout, since a badge write can legitimately take longer than the
	// timeout that's appropriate for every other endpoint.
	if err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(nfcWriteBudget)); err != nil {
		slog.Warn("extend NFC write deadline", "error", err)
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	flusher, canFlush := writer.(http.Flusher)

	emit := func(stage string, percent int, message string) {
		payload, err := json.Marshal(nfcEvent{Stage: stage, Percent: percent, Message: message})
		if err != nil {
			slog.Warn("encode NFC progress event", "error", err)
			return
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", payload); err != nil {
			slog.Warn("write NFC progress event", "error", err)
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}

	ctx, cancel := context.WithTimeout(request.Context(), nfcWriteBudget)
	defer cancel()

	if err := service.nfcWriter.Write(ctx, img, profile, emit); err != nil {
		slog.Info("NFC badge write failed", "error", err)
		emit("error", 0, userFacingNFCWriteError(profile, err))
		return
	}
	emit("done", 100, "Badge write completed.")
}

func (service *api) readNFCTagInfo(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}

	select {
	case service.nfcSlots <- struct{}{}:
		defer func() { <-service.nfcSlots }()
	case <-request.Context().Done():
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), nfcTagInfoBudget)
	defer cancel()

	info, err := service.nfcTagInfo.ReadInfo(ctx)
	if err != nil {
		slog.Info("NFC tag info read failed", "error", err)
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, info)
}

type nfcTagNDEFWriteRequest struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (service *api) writeNFCTagNDEF(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-WebBadge-Request") != "1" {
		writeError(writer, http.StatusForbidden, "request header is missing")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload nfcTagNDEFWriteRequest
	if err := decoder.Decode(&payload); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid NDEF write request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid NDEF write request")
		return
	}

	select {
	case service.nfcSlots <- struct{}{}:
		defer func() { <-service.nfcSlots }()
	case <-request.Context().Done():
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), nfcTagInfoBudget)
	defer cancel()
	if err := service.nfcTagWrite.WriteNDEF(ctx, payload.Type, payload.Value); err != nil {
		slog.Info("NFC tag NDEF write failed", "error", err)
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func userFacingNFCWriteError(profile badgeimage.Profile, err error) string {
	if message, ok := classifyWrongNFCAreaError(profile, err); ok {
		return message
	}
	return err.Error()
}

func classifyWrongNFCAreaError(profile badgeimage.Profile, err error) (string, bool) {
	message := err.Error()
	switch profile.ID {
	case badgeimage.Hybrid397.ID:
		if strings.Contains(message, "ACR1552 transparent exchange failed") ||
			strings.Contains(message, "wake badge: write FIFO") {
			return "The badge was detected, but the reader reached its regular NFC tag area instead of the e-ink image-writing interface. Reposition the badge and try again.", true
		}
	case badgeimage.NfcInk240x416.ID:
		if strings.Contains(message, "read nfcink config:") ||
			strings.Contains(message, "write nfcink image:") ||
			strings.Contains(message, "refresh nfcink badge:") {
			return "The badge was detected, but it did not answer like the nfcink image-writing interface. If this badge also exposes normal NFC tag data, place the image-writing area on the reader and try again.", true
		}
	}
	return "", false
}
