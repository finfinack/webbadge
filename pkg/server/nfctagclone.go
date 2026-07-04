package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"webbadge/pkg/pcsc"
)

// nfcCloneBudget bounds the whole clone request: waiting for the source tag,
// reading it, waiting for the operator to swap in the destination tag, and
// writing it. Each individual WaitForCard call still uses nfcCardWaitTimeout.
const nfcCloneBudget = 3 * time.Minute

const (
	// classicMaxSectors caps a MIFARE Classic sweep at the largest common
	// format (4K, 40 sectors) so a malformed/foreign card can't spin forever.
	classicMaxSectors = 40
	// classicMaxAuthMisses stops the sweep once we hit this many consecutive
	// sectors that don't authenticate with a known key, treating that as
	// "past the end of the card" rather than probing all 40 unconditionally.
	classicMaxAuthMisses = 3
	// type2MaxPages caps a Type 2 page sweep as a safety net against a card
	// that never returns a bad status.
	type2MaxPages      = 256
	type2DataStartPage = 4
	type2PagesPerRead  = 16
)

// classicDefaultKeys are the well-known, publicly documented MIFARE Classic
// keys (factory default and the NFC Forum default). Cloning never uses any
// other key material, so a sector trailer is only ever written back when it
// already used one of these keys on the source tag.
var classicDefaultKeys = [][]byte{
	{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
	{0xD3, 0xF7, 0xD3, 0xF7, 0xD3, 0xF7},
}

type tagFamily int

const (
	familyUnknown tagFamily = iota
	familyType4
	familyType2
	familyMifareClassic
)

func (family tagFamily) String() string {
	switch family {
	case familyType4:
		return "Type 4 (NDEF filesystem)"
	case familyType2:
		return "Type 2 (Ultralight/NTAG)"
	case familyMifareClassic:
		return "MIFARE Classic"
	default:
		return "unknown"
	}
}

// classicSectorDump is one MIFARE Classic sector as read from the source
// tag. Block 0 of sector 0 (the factory manufacturer block) is never
// captured here — it can't be cloned on a genuine tag.
type classicSectorDump struct {
	Sector           int
	Key              []byte
	Blocks           map[int][]byte // block number -> 16 raw bytes
	Trailer          []byte         // 16-byte trailer as read, nil if not captured
	TrailerIsDefault bool           // true only if both trailer keys are known defaults
}

// tagRawDump is the family-specific raw memory captured from a source tag,
// ready to be written back to a destination tag of the same family.
type tagRawDump struct {
	Family tagFamily

	// Type 4
	NDEFPayload []byte

	// Type 2 — page-addressed memory, captured from type2DataStartPage
	// onward (UID pages 0-1 and the lock/internal page 2 are never touched).
	Type2StartPage int
	Type2Pages     [][]byte

	// MIFARE Classic
	ClassicSectors []classicSectorDump
}

func dumpSummary(dump tagRawDump) string {
	switch dump.Family {
	case familyType4:
		return fmt.Sprintf("%d NDEF bytes", len(dump.NDEFPayload))
	case familyType2:
		return fmt.Sprintf("%d pages", len(dump.Type2Pages))
	case familyMifareClassic:
		return fmt.Sprintf("%d sectors", len(dump.ClassicSectors))
	default:
		return ""
	}
}

// classifyTag determines which of the tag families this codebase understands
// a card belongs to, trying the same fallback order ReadInfo already uses
// for NDEF (Type 4, then Type 2) and adding MIFARE Classic as a third,
// key-based fallback.
func classifyTag(ctx context.Context, card *pcsc.Card) (tagFamily, error) {
	if _, err := readNDEFCapability(ctx, card); err == nil {
		return familyType4, nil
	}
	if isType2Tag(ctx, card) {
		return familyType2, nil
	}
	if _, ok := classicAuthenticate(ctx, card, 0, 0x60); ok {
		return familyMifareClassic, nil
	}
	return familyUnknown, errors.New("could not determine tag type for cloning")
}

func isType2Tag(ctx context.Context, card *pcsc.Card) bool {
	data, ok := tryTransmitData(ctx, card, []byte{0xFF, 0xB0, 0x00, 0x03, 0x04})
	return ok && len(data) == 4 && data[0] == 0xE1 && (data[1]&0xF0) == 0x10
}

// tryTransmitData is like transmitData but reports failure instead of
// returning an error, since callers use it to probe for "does this
// tag/page/sector exist" rather than to perform a required operation.
func tryTransmitData(ctx context.Context, card *pcsc.Card, command []byte) ([]byte, bool) {
	response, err := card.Transmit(ctx, command)
	if err != nil || len(response) < 2 {
		return nil, false
	}
	sw := response[len(response)-2:]
	if sw[0] != 0x90 || sw[1] != 0x00 {
		return nil, false
	}
	return response[:len(response)-2], true
}

func classicAuthenticate(ctx context.Context, card *pcsc.Card, block byte, keyType byte) ([]byte, bool) {
	for _, key := range classicDefaultKeys {
		if classicAuthenticateWithKey(ctx, card, block, keyType, key) {
			return key, true
		}
	}
	return nil, false
}

func classicAuthenticateWithKey(ctx context.Context, card *pcsc.Card, block byte, keyType byte, key []byte) bool {
	loadKey := append([]byte{0xFF, 0x82, 0x00, 0x00, byte(len(key))}, key...)
	if _, ok := tryTransmitData(ctx, card, loadKey); !ok {
		return false
	}
	auth := []byte{0xFF, 0x86, 0x00, 0x00, 0x05, 0x01, 0x00, block, keyType, 0x00}
	_, ok := tryTransmitData(ctx, card, auth)
	return ok
}

func classicKeyIsDefault(key []byte) bool {
	for _, candidate := range classicDefaultKeys {
		if bytes.Equal(candidate, key) {
			return true
		}
	}
	return false
}

// readType4Dump captures the NDEF file's raw bytes verbatim — no decoding
// into individual records, so multi-record messages and non-text/url/email
// records are preserved exactly.
func readType4Dump(ctx context.Context, card *pcsc.Card) (tagRawDump, error) {
	capability, err := readNDEFCapability(ctx, card)
	if err != nil {
		return tagRawDump{}, fmt.Errorf("read NDEF capability: %w", err)
	}
	payload, err := readSelectedNDEFContent(ctx, card, capability)
	if err != nil {
		return tagRawDump{}, fmt.Errorf("read NDEF content: %w", err)
	}
	return tagRawDump{Family: familyType4, NDEFPayload: payload}, nil
}

func writeType4Dump(ctx context.Context, card *pcsc.Card, dump tagRawDump) error {
	capability, err := readNDEFCapability(ctx, card)
	if err != nil {
		return fmt.Errorf("read destination NDEF capability: %w", err)
	}
	if !capability.Writable {
		return errors.New("destination tag exposes NDEF but is not writable")
	}
	return writeNDEFFile(ctx, card, capability, dump.NDEFPayload)
}

// readType2Dump reads every user-memory page it can, not just the NDEF TLV
// wrapper, so data stored outside NDEF is captured too. It stops at the
// first page read that the tag rejects, which in practice means "past the
// end of memory" for a well-formed tag.
func readType2Dump(ctx context.Context, card *pcsc.Card) (tagRawDump, error) {
	pages := make([][]byte, 0, 64)
	for page := type2DataStartPage; page < type2DataStartPage+type2MaxPages; {
		length := type2PagesPerRead * 4
		data, ok := tryTransmitData(ctx, card, []byte{0xFF, 0xB0, 0x00, byte(page), byte(length)})
		if !ok || len(data) == 0 {
			break
		}
		readPages := len(data) / 4
		for offset := 0; offset+4 <= len(data); offset += 4 {
			pages = append(pages, append([]byte(nil), data[offset:offset+4]...))
		}
		page += readPages
		if readPages < type2PagesPerRead {
			break
		}
	}
	if len(pages) == 0 {
		return tagRawDump{}, errors.New("could not read any type 2 pages")
	}
	return tagRawDump{Family: familyType2, Type2StartPage: type2DataStartPage, Type2Pages: pages}, nil
}

func writeType2Dump(ctx context.Context, card *pcsc.Card, dump tagRawDump) error {
	totalPages, err := type2Capacity(ctx, card)
	if err != nil {
		return fmt.Errorf("check destination capacity: %w", err)
	}
	needed := dump.Type2StartPage + len(dump.Type2Pages)
	if needed > totalPages {
		return fmt.Errorf("destination tag has %d pages, source needs %d", totalPages, needed)
	}
	for index, pageBytes := range dump.Type2Pages {
		page := dump.Type2StartPage + index
		command := append([]byte{0xFF, 0xD6, 0x00, byte(page), 0x04}, pageBytes...)
		if err := transmitOK(ctx, card, command, fmt.Sprintf("write type 2 page %d", page)); err != nil {
			return err
		}
	}
	return nil
}

func type2Capacity(ctx context.Context, card *pcsc.Card) (int, error) {
	data, ok := tryTransmitData(ctx, card, []byte{0xFF, 0xB0, 0x00, 0x03, 0x04})
	if !ok || len(data) < 4 {
		return 0, errors.New("read destination capability container")
	}
	if data[0] != 0xE1 || (data[1]&0xF0) != 0x10 {
		return 0, errors.New("destination capability container not found")
	}
	capacityBytes := int(data[2]) * 8
	return type2DataStartPage + capacityBytes/4, nil
}

// readClassicDump sweeps sectors from 0 upward, authenticating with the same
// well-known default keys used elsewhere in this file. Sector 0's block 0
// (the manufacturer block) is never captured. A sector that won't
// authenticate with a known key is skipped, not fatal — the clone proceeds
// with whichever sectors it could read.
func readClassicDump(ctx context.Context, card *pcsc.Card) (tagRawDump, error) {
	sectors := make([]classicSectorDump, 0, 16)
	consecutiveMisses := 0
	for sector := 0; sector < classicMaxSectors && consecutiveMisses < classicMaxAuthMisses; sector++ {
		firstBlock := sector * 4
		key, ok := classicAuthenticate(ctx, card, byte(firstBlock), 0x60)
		if !ok {
			consecutiveMisses++
			continue
		}
		consecutiveMisses = 0

		dump := classicSectorDump{Sector: sector, Key: key, Blocks: make(map[int][]byte)}
		readFailed := false
		for block := 0; block < 4; block++ {
			blockNumber := firstBlock + block
			if sector == 0 && block == 0 {
				continue
			}
			data, ok := tryTransmitData(ctx, card, []byte{0xFF, 0xB0, 0x00, byte(blockNumber), 0x10})
			if !ok || len(data) != 16 {
				readFailed = true
				break
			}
			if block == 3 {
				dump.Trailer = data
				dump.TrailerIsDefault = classicKeyIsDefault(data[:6]) && classicKeyIsDefault(data[10:16])
			} else {
				dump.Blocks[blockNumber] = data
			}
		}
		if readFailed {
			continue
		}
		sectors = append(sectors, dump)
	}
	if len(sectors) == 0 {
		return tagRawDump{}, errors.New("could not read any MIFARE Classic sectors with known keys")
	}
	return tagRawDump{Family: familyMifareClassic, ClassicSectors: sectors}, nil
}

// writeClassicDump re-authenticates to the destination with the exact key
// that worked on the source, writes the data blocks, and — only when the
// source trailer itself used a known default key — writes the trailer back
// too. Block 0 is never written under any circumstance.
func writeClassicDump(ctx context.Context, card *pcsc.Card, dump tagRawDump) error {
	written := 0
	for _, sector := range dump.ClassicSectors {
		firstBlock := sector.Sector * 4
		if !classicAuthenticateWithKey(ctx, card, byte(firstBlock), 0x60, sector.Key) {
			continue
		}
		for block, data := range sector.Blocks {
			command := append([]byte{0xFF, 0xD6, 0x00, byte(block), 0x10}, data...)
			if err := transmitOK(ctx, card, command, fmt.Sprintf("write MIFARE block %d", block)); err != nil {
				return err
			}
		}
		if sector.TrailerIsDefault && len(sector.Trailer) == 16 {
			trailerBlock := firstBlock + 3
			command := append([]byte{0xFF, 0xD6, 0x00, byte(trailerBlock), 0x10}, sector.Trailer...)
			if err := transmitOK(ctx, card, command, fmt.Sprintf("write MIFARE trailer for sector %d", sector.Sector)); err != nil {
				return err
			}
		}
		written++
	}
	if written == 0 {
		return errors.New("could not authenticate to any destination sector with the source's keys")
	}
	return nil
}

func readTagDump(ctx context.Context, card *pcsc.Card, family tagFamily) (tagRawDump, error) {
	switch family {
	case familyType4:
		return readType4Dump(ctx, card)
	case familyType2:
		return readType2Dump(ctx, card)
	case familyMifareClassic:
		return readClassicDump(ctx, card)
	default:
		return tagRawDump{}, fmt.Errorf("unsupported tag family")
	}
}

func writeTagDump(ctx context.Context, card *pcsc.Card, dump tagRawDump) error {
	switch dump.Family {
	case familyType4:
		return writeType4Dump(ctx, card, dump)
	case familyType2:
		return writeType2Dump(ctx, card, dump)
	case familyMifareClassic:
		return writeClassicDump(ctx, card, dump)
	default:
		return fmt.Errorf("unsupported tag family")
	}
}

// nfcTagCloner reads a source tag and writes its raw memory to a destination
// tag placed on the reader afterward, reporting stage/percent/message
// progress (including the prompt to swap tags) as it goes.
type nfcTagCloner interface {
	Clone(ctx context.Context, report func(stage string, percent int, message string)) error
}

type pcscNFCTagCloner struct{}

func (pcscNFCTagCloner) Clone(ctx context.Context, report func(stage string, percent int, message string)) error {
	session, err := pcsc.Establish()
	if err != nil {
		return fmt.Errorf("establish PC/SC session: %w", err)
	}
	defer session.Close()

	reader, err := session.SelectReader("")
	if err != nil {
		return fmt.Errorf("select PC/SC reader: %w", err)
	}

	report("waiting-source", 0, fmt.Sprintf("Place the source tag on %s…", reader))
	sourceCtx, cancel := context.WithTimeout(ctx, nfcCardWaitTimeout)
	sourceCard, err := session.WaitForCard(sourceCtx, reader, 200*time.Millisecond)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for source tag: %w", err)
	}

	family, err := classifyTag(ctx, sourceCard)
	if err != nil {
		sourceCard.Close()
		return fmt.Errorf("classify source tag: %w", err)
	}

	report("reading", 15, fmt.Sprintf("Reading %s tag…", family))
	dump, err := readTagDump(ctx, sourceCard, family)
	sourceCard.Close()
	if err != nil {
		return fmt.Errorf("read source tag: %w", err)
	}

	report("source-read", 40, fmt.Sprintf(
		"Read %s tag (%s). Remove it and place the destination tag on %s…", family, dumpSummary(dump), reader))

	destCtx, cancel := context.WithTimeout(ctx, nfcCardWaitTimeout)
	destCard, err := session.WaitForCard(destCtx, reader, 200*time.Millisecond)
	cancel()
	if err != nil {
		return fmt.Errorf("wait for destination tag: %w", err)
	}
	defer destCard.Close()

	destFamily, err := classifyTag(ctx, destCard)
	if err != nil {
		return fmt.Errorf("classify destination tag: %w", err)
	}
	if destFamily != family {
		return fmt.Errorf("destination tag is %s, but the source tag was %s; use a matching tag type", destFamily, family)
	}

	report("writing", 60, "Writing to destination tag…")
	if err := writeTagDump(ctx, destCard, dump); err != nil {
		return fmt.Errorf("write destination tag: %w", err)
	}
	return nil
}

func (service *api) writeNFCTagClone(writer http.ResponseWriter, request *http.Request) {
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

	if err := http.NewResponseController(writer).SetWriteDeadline(time.Now().Add(nfcCloneBudget)); err != nil {
		slog.Warn("extend NFC clone write deadline", "error", err)
	}

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	flusher, canFlush := writer.(http.Flusher)

	emit := func(stage string, percent int, message string) {
		payload, err := json.Marshal(nfcEvent{Stage: stage, Percent: percent, Message: message})
		if err != nil {
			slog.Warn("encode NFC clone progress event", "error", err)
			return
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", payload); err != nil {
			slog.Warn("write NFC clone progress event", "error", err)
			return
		}
		if canFlush {
			flusher.Flush()
		}
	}

	ctx, cancel := context.WithTimeout(request.Context(), nfcCloneBudget)
	defer cancel()

	if err := service.nfcTagClone.Clone(ctx, emit); err != nil {
		slog.Info("NFC tag clone failed", "error", err)
		emit("error", 0, err.Error())
		return
	}
	emit("done", 100, "Tag clone completed.")
}
