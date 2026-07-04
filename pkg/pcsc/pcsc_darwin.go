//go:build darwin && cgo

// Package pcsc provides the small subset of PC/SC needed by the CLI.
package pcsc

/*
#cgo darwin LDFLAGS: -framework PCSC
#include <PCSC/wintypes.h>
#include <PCSC/winscard.h>
#include <stdlib.h>

static LONG transmit_card(SCARDHANDLE card, DWORD protocol,
	const unsigned char *send, DWORD sendLen, unsigned char *recv, DWORD *recvLen) {
	SCARD_IO_REQUEST sendPCI;
	sendPCI.dwProtocol = protocol;
	sendPCI.cbPciLength = sizeof(SCARD_IO_REQUEST);
	return SCardTransmit(card, &sendPCI, send, sendLen, NULL, recv, recvLen);
}

static LONG control_card_legacy(SCARDHANDLE card,
	const unsigned char *send, DWORD sendLen, unsigned char *recv, DWORD *recvLen) {
#undef SCardControl
	return SCardControl(card, send, sendLen, recv, recvLen);
}

static LONG control_card_132(SCARDHANDLE card, DWORD controlCode,
	const unsigned char *send, DWORD sendLen, unsigned char *recv, DWORD recvCap, DWORD *recvLen) {
	return SCardControl132(card, controlCode, send, sendLen, recv, recvCap, recvLen);
}

static LONG card_atr(SCARDHANDLE card, unsigned char *atr, DWORD *atrLen) {
	DWORD state = 0;
	DWORD protocol = 0;
	DWORD readerLen = 0;
	return SCardStatus(card, NULL, &readerLen, &state, &protocol, atr, atrLen);
}

static LONG connect_direct_reader(SCARDCONTEXT context, const char *reader,
	SCARDHANDLE *handle, DWORD *activeProtocol) {
	return SCardConnect(context, reader, SCARD_SHARE_DIRECT, SCARD_PROTOCOL_RAW, handle, activeProtocol);
}

static LONG connect_direct_reader_zero(SCARDCONTEXT context, const char *reader,
	SCARDHANDLE *handle, DWORD *activeProtocol) {
	return SCardConnect(context, reader, SCARD_SHARE_DIRECT, 0, handle, activeProtocol);
}

static LONG no_smartcard_code(void) {
	return SCARD_E_NO_SMARTCARD;
}

*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unsafe"
)

const maxResponseLength = 65538

// Session owns a PC/SC resource-manager context.
type Session struct {
	handle C.SCARDCONTEXT
}

// Card is a connected contactless smart card.
type Card struct {
	context  C.SCARDCONTEXT
	handle   C.SCARDHANDLE
	protocol C.DWORD
	reader   string
	atr      []byte
}

// Establish creates a PC/SC session.
func Establish() (*Session, error) {
	var handle C.SCARDCONTEXT
	result := C.SCardEstablishContext(C.SCARD_SCOPE_SYSTEM, nil, nil, &handle)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardEstablishContext", result)
	}
	return &Session{handle: handle}, nil
}

// Close releases the PC/SC session.
func (session *Session) Close() {
	if session == nil || session.handle == 0 {
		return
	}
	C.SCardReleaseContext(session.handle)
	session.handle = 0
}

// Readers returns all attached PC/SC reader names.
func (session *Session) Readers() ([]string, error) {
	var length C.DWORD
	result := C.SCardListReaders(session.handle, nil, nil, &length)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardListReaders(size)", result)
	}
	if length == 0 {
		return nil, nil
	}
	buffer := C.malloc(C.size_t(length))
	if buffer == nil {
		return nil, errors.New("allocate PC/SC reader list")
	}
	defer C.free(buffer)
	result = C.SCardListReaders(session.handle, nil, (*C.char)(buffer), &length)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardListReaders", result)
	}
	raw := C.GoBytes(buffer, C.int(length))
	var readers []string
	for len(raw) > 0 {
		index := 0
		for index < len(raw) && raw[index] != 0 {
			index++
		}
		if index == 0 {
			break
		}
		readers = append(readers, string(raw[:index]))
		raw = raw[index+1:]
	}
	return readers, nil
}

// SelectReader chooses a PICC reader by exact name or substring.
func (session *Session) SelectReader(query string) (string, error) {
	readers, err := session.Readers()
	if err != nil {
		return "", err
	}
	if len(readers) == 0 {
		return "", errors.New("no PC/SC readers found")
	}
	if query != "" {
		var matches []string
		for _, reader := range readers {
			if reader == query || strings.Contains(strings.ToLower(reader), strings.ToLower(query)) {
				matches = append(matches, reader)
			}
		}
		switch len(matches) {
		case 0:
			return "", fmt.Errorf("no reader matches %q; available: %s", query, strings.Join(readers, ", "))
		case 1:
			return matches[0], nil
		default:
			return "", fmt.Errorf("reader query %q is ambiguous: %s", query, strings.Join(matches, ", "))
		}
	}
	for _, reader := range readers {
		lower := strings.ToLower(reader)
		if strings.Contains(lower, "acr1552") && strings.Contains(lower, "picc") {
			return reader, nil
		}
	}
	for _, reader := range readers {
		if strings.Contains(strings.ToLower(reader), "picc") {
			return reader, nil
		}
	}
	return readers[0], nil
}

// WaitForCard polls until a card is present or the context expires.
func (session *Session) WaitForCard(ctx context.Context, reader string, interval time.Duration) (*Card, error) {
	if interval <= 0 {
		return nil, errors.New("poll interval must be positive")
	}
	for {
		card, err := session.connect(reader)
		if err == nil {
			return card, nil
		}
		if !errors.Is(err, errNoSmartcard) {
			return nil, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (session *Session) connect(reader string) (*Card, error) {
	readerName := C.CString(reader)
	defer C.free(unsafe.Pointer(readerName))
	var handle C.SCARDHANDLE
	var protocol C.DWORD
	result := C.SCardConnect(session.handle, readerName, C.SCARD_SHARE_SHARED, C.SCARD_PROTOCOL_T0|C.SCARD_PROTOCOL_T1, &handle, &protocol)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardConnect", result)
	}
	card := &Card{context: session.handle, handle: handle, protocol: protocol, reader: reader}
	atr, err := card.readATR()
	if err != nil {
		card.Close()
		return nil, err
	}
	card.atr = atr
	slog.Debug("PC/SC card connected",
		"reader", reader,
		"protocol", uint32(protocol),
		"atr", fmt.Sprintf("%X", atr))
	return card, nil
}

// Close disconnects from the card without resetting it.
func (card *Card) Close() {
	if card == nil || card.handle == 0 {
		return
	}
	C.SCardDisconnect(card.handle, C.SCARD_LEAVE_CARD)
	card.handle = 0
}

// Reader returns the PC/SC reader name.
func (card *Card) Reader() string {
	return card.reader
}

// ATR returns a copy of the card's answer-to-reset.
func (card *Card) ATR() []byte {
	return append([]byte(nil), card.atr...)
}

// Transmit exchanges one APDU with the card.
func (card *Card) Transmit(ctx context.Context, command []byte) ([]byte, error) {
	if len(command) == 0 {
		return nil, errors.New("APDU is empty")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	response := make([]byte, maxResponseLength)
	responseLength := C.DWORD(len(response))
	result := C.transmit_card(
		card.handle,
		card.protocol,
		(*C.uchar)(unsafe.Pointer(&command[0])),
		C.DWORD(len(command)),
		(*C.uchar)(unsafe.Pointer(&response[0])),
		&responseLength,
	)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardTransmit", result)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return response[:int(responseLength)], nil
}

func (card *Card) readATR() ([]byte, error) {
	atr := make([]byte, 64)
	length := C.DWORD(len(atr))
	result := C.card_atr(card.handle, (*C.uchar)(unsafe.Pointer(&atr[0])), &length)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardStatus", result)
	}
	return atr[:int(length)], nil
}

// ControlLegacy exchanges a reader control command using the legacy pcsc-lite
// SCardControl signature available on macOS.
func (card *Card) ControlLegacy(ctx context.Context, command []byte) ([]byte, error) {
	if len(command) == 0 {
		return nil, errors.New("control command is empty")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	response, err := card.controlLegacyHandle(ctx, card.handle, command)
	if err == nil {
		return response, nil
	}
	directHandle, release, err := card.openDirectHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	return card.controlLegacyHandle(ctx, directHandle, command)
}

func (card *Card) controlLegacyHandle(ctx context.Context, handle C.SCARDHANDLE, command []byte) ([]byte, error) {
	response := make([]byte, maxResponseLength)
	responseLength := C.DWORD(len(response))
	result := C.control_card_legacy(
		handle,
		(*C.uchar)(unsafe.Pointer(&command[0])),
		C.DWORD(len(command)),
		(*C.uchar)(unsafe.Pointer(&response[0])),
		&responseLength,
	)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardControl", result)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return response[:int(responseLength)], nil
}

// Control exchanges a reader control command with an explicit control code.
func (card *Card) Control(ctx context.Context, controlCode uint32, command []byte) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	response, err := card.controlHandle(ctx, card.handle, controlCode, command)
	if err == nil {
		return response, nil
	}
	directHandle, release, err := card.openDirectHandle()
	if err != nil {
		return nil, err
	}
	defer release()
	return card.controlHandle(ctx, directHandle, controlCode, command)
}

func (card *Card) controlHandle(ctx context.Context, handle C.SCARDHANDLE, controlCode uint32, command []byte) ([]byte, error) {
	response := make([]byte, maxResponseLength)
	responseLength := C.DWORD(len(response))
	var sendPtr *C.uchar
	if len(command) > 0 {
		sendPtr = (*C.uchar)(unsafe.Pointer(&command[0]))
	}
	result := C.control_card_132(
		handle,
		C.DWORD(controlCode),
		sendPtr,
		C.DWORD(len(command)),
		(*C.uchar)(unsafe.Pointer(&response[0])),
		C.DWORD(len(response)),
		&responseLength,
	)
	if result != C.SCARD_S_SUCCESS {
		return nil, pcscError("SCardControl132", result)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return response[:int(responseLength)], nil
}

func (card *Card) openDirectHandle() (C.SCARDHANDLE, func(), error) {
	readerName := C.CString(card.reader)
	defer C.free(unsafe.Pointer(readerName))

	var handle C.SCARDHANDLE
	var protocol C.DWORD
	result := C.connect_direct_reader_zero(card.context, readerName, &handle, &protocol)
	if result == C.SCARD_S_SUCCESS {
		return handle, func() {
			C.SCardDisconnect(handle, C.SCARD_LEAVE_CARD)
		}, nil
	}

	result = C.connect_direct_reader(card.context, readerName, &handle, &protocol)
	if result != C.SCARD_S_SUCCESS {
		return 0, nil, pcscError("SCardConnect(direct)", result)
	}
	return handle, func() {
		C.SCardDisconnect(handle, C.SCARD_LEAVE_CARD)
	}, nil
}

var errNoSmartcard = errors.New("no smart card present")

func pcscError(operation string, code C.LONG) error {
	if code == C.no_smartcard_code() {
		return fmt.Errorf("%s: %w", operation, errNoSmartcard)
	}
	return fmt.Errorf("%s failed with PC/SC code 0x%08X", operation, uint32(code))
}
