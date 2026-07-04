package nfcbadge

import (
	"context"
	"errors"
	"testing"
)

type scriptedCard struct {
	commands  [][]byte
	responses [][]byte
	err       error
}

func (card *scriptedCard) Transmit(_ context.Context, command []byte) ([]byte, error) {
	card.commands = append(card.commands, append([]byte(nil), command...))
	if card.err != nil {
		return nil, card.err
	}
	if len(card.responses) == 0 {
		return []byte{0x90, 0x00}, nil
	}
	response := card.responses[0]
	card.responses = card.responses[1:]
	return append([]byte(nil), response...), nil
}

func (card *scriptedCard) ATR() []byte    { return []byte{0x3B, 0x00} }
func (card *scriptedCard) Reader() string { return "test reader" }
func (card *scriptedCard) Control(_ context.Context, _ uint32, command []byte) ([]byte, error) {
	card.commands = append(card.commands, append([]byte(nil), command...))
	if card.err != nil {
		return nil, card.err
	}
	if len(card.responses) == 0 {
		return []byte{}, nil
	}
	response := card.responses[0]
	card.responses = card.responses[1:]
	return append([]byte(nil), response...), nil
}

func TestTransmitOK(t *testing.T) {
	tests := []struct {
		name      string
		response  []byte
		wantData  []byte
		wantError bool
	}{
		{name: "success", response: []byte{0x01, 0x02, 0x90, 0x00}, wantData: []byte{0x01, 0x02}},
		{name: "card error", response: []byte{0x69, 0x86}, wantError: true},
		{name: "short", response: []byte{0x90}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			card := &scriptedCard{responses: [][]byte{test.response}}
			got, err := transmitOK(context.Background(), card, []byte{0x00})
			if (err != nil) != test.wantError {
				t.Fatalf("transmitOK() error = %v, wantError %v", err, test.wantError)
			}
			if !equalBytes(got, test.wantData) {
				t.Fatalf("transmitOK() = %X, want %X", got, test.wantData)
			}
		})
	}
}

func TestPackageImageBlock(t *testing.T) {
	source := make([]byte, 248)
	result := packageImageBlock(source)
	if result[0] != 0xF7 || result[3] != 247 {
		t.Fatalf("first header = %X, want F7 0000 F7", result[:4])
	}
	second := 4 + 247
	if result[second] != 0x77 || result[second+1] != 0 || result[second+2] != 247 || result[second+3] != 1 {
		t.Fatalf("second header = %X, want 77 00F7 01", result[second:second+4])
	}
}

func TestParseTransparentResponse(t *testing.T) {
	tests := []struct {
		name      string
		response  []byte
		want      []byte
		wantError bool
	}{
		{
			name:     "card response",
			response: []byte{0xC0, 0x03, 0x00, 0x90, 0x00, 0x92, 0x01, 0x00, 0x96, 0x02, 0x00, 0x00, 0x97, 0x02, 0xAA, 0xAA},
			want:     []byte{0xAA, 0xAA},
		},
		{
			name:     "unknown multi-byte tag",
			response: []byte{0x5F, 0x51, 0x02, 0x01, 0x02, 0x97, 0x02, 0xAA, 0xAA},
			want:     []byte{0xAA, 0xAA},
		},
		{
			name:      "RF error",
			response:  []byte{0xC0, 0x03, 0x00, 0x90, 0x00, 0x96, 0x02, 0x01, 0x00},
			wantError: true,
		},
		{
			name:      "truncated",
			response:  []byte{0x97, 0x02, 0xAA},
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseTransparentResponse(test.response)
			if (err != nil) != test.wantError {
				t.Fatalf("parseTransparentResponse() error = %v, wantError %v", err, test.wantError)
			}
			if !equalBytes(got, test.want) {
				t.Fatalf("parseTransparentResponse() = %X, want %X", got, test.want)
			}
		})
	}
}

func TestTransmitErrorPropagates(t *testing.T) {
	card := &scriptedCard{err: errors.New("transport failed")}
	_, err := transmitOK(context.Background(), card, []byte{0x00})
	if err == nil {
		t.Fatal("transmitOK() error = nil")
	}
}

func TestShouldRetryTransparent(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "6300", err: &StatusError{Status: 0x6300}, want: true},
		{name: "6900", err: &StatusError{Status: 0x6900}, want: true},
		{name: "6A81", err: &StatusError{Status: 0x6A81}, want: true},
		{name: "6D00", err: &StatusError{Status: 0x6D00}, want: true},
		{name: "other status", err: &StatusError{Status: 0x6982}, want: false},
		{name: "other error", err: errors.New("transport failed"), want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldRetryTransparent(test.err); got != test.want {
				t.Fatalf("shouldRetryTransparent(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestBuildTransparentExchangeAPDU(t *testing.T) {
	data := []byte{0x90, 0x02, 0x10, 0x00}

	withLe, err := buildTransparentExchangeAPDU(data, transparentAPDUShortWithLe)
	if err != nil {
		t.Fatalf("buildTransparentExchangeAPDU(..., shortWithLe) error = %v", err)
	}
	if !equalBytes(withLe, []byte{0xFF, 0xC2, 0x00, 0x01, 0x04, 0x90, 0x02, 0x10, 0x00, 0x00}) {
		t.Fatalf("buildTransparentExchangeAPDU(..., true) = %X", withLe)
	}

	withoutLe, err := buildTransparentExchangeAPDU(data, transparentAPDUShortNoLe)
	if err != nil {
		t.Fatalf("buildTransparentExchangeAPDU(..., shortNoLe) error = %v", err)
	}
	if !equalBytes(withoutLe, []byte{0xFF, 0xC2, 0x00, 0x01, 0x04, 0x90, 0x02, 0x10, 0x00}) {
		t.Fatalf("buildTransparentExchangeAPDU(..., false) = %X", withoutLe)
	}

	extendedNoLe, err := buildTransparentExchangeAPDU(data, transparentAPDUExtendedNoLe)
	if err != nil {
		t.Fatalf("buildTransparentExchangeAPDU(..., extendedNoLe) error = %v", err)
	}
	if !equalBytes(extendedNoLe, []byte{0xFF, 0xC2, 0x00, 0x01, 0x00, 0x00, 0x04, 0x90, 0x02, 0x10, 0x00}) {
		t.Fatalf("buildTransparentExchangeAPDU(..., extendedNoLe) = %X", extendedNoLe)
	}
}

func TestRawEscapeAPDUCandidates(t *testing.T) {
	candidates := rawEscapeAPDUCandidates([]byte{0x11, 0x22})
	if len(candidates) != 4 {
		t.Fatalf("rawEscapeAPDUCandidates() returned %d candidates, want 4", len(candidates))
	}
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "ff-c2-p2-00+le", payload: []byte{0xFF, 0xC2, 0x00, 0x00, 0x02, 0x11, 0x22, 0x00}},
		{name: "ff-c2-p2-00-no-le", payload: []byte{0xFF, 0xC2, 0x00, 0x00, 0x02, 0x11, 0x22}},
		{name: "ff-c2-p2-01+le", payload: []byte{0xFF, 0xC2, 0x00, 0x01, 0x02, 0x11, 0x22, 0x00}},
		{name: "ff-c2-p2-01-no-le", payload: []byte{0xFF, 0xC2, 0x00, 0x01, 0x02, 0x11, 0x22}},
	}
	for index, test := range tests {
		if candidates[index].name != test.name {
			t.Fatalf("candidate %d name = %q, want %q", index, candidates[index].name, test.name)
		}
		if !equalBytes(candidates[index].payload, test.payload) {
			t.Fatalf("candidate %d payload = %X, want %X", index, candidates[index].payload, test.payload)
		}
	}
}

func TestTransparentExchangeUsesRawEscapeBeforeNestedTransparentExchange(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0xAA, 0xAA, 0x90, 0x00},
	}}

	writer := &NFCA397Writer{card: card, transport: nfcaTransportTransparent}
	got, err := writer.transparentExchange(context.Background(), []byte{0x11, 0x22})
	if err != nil {
		t.Fatalf("transparentExchange() error = %v", err)
	}
	if !equalBytes(got, []byte{0xAA, 0xAA}) {
		t.Fatalf("transparentExchange() = %X, want AAAA", got)
	}
	if len(card.commands) != 1 {
		t.Fatalf("transparentExchange() sent %d commands, want 1", len(card.commands))
	}
	if !equalBytes(card.commands[0], []byte{0xFF, 0xC2, 0x00, 0x00, 0x02, 0x11, 0x22, 0x00}) {
		t.Fatalf("first command = %X, want raw escape APDU", card.commands[0])
	}
}

func TestTransparentExchangeRetriesWithoutLeOn6700(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0x67, 0x00},
		{0x67, 0x00},
		{0x67, 0x00},
		{0x67, 0x00},
		{0x67, 0x00},
		{0x67, 0x00},
		{0xC0, 0x03, 0x00, 0x90, 0x00, 0x92, 0x01, 0x00, 0x96, 0x02, 0x00, 0x00, 0x97, 0x02, 0xAA, 0xAA, 0x90, 0x00},
	}}

	writer := &NFCA397Writer{card: card, transport: nfcaTransportTransparent}
	got, err := writer.transparentExchange(context.Background(), []byte{0x11, 0x22})
	if err != nil {
		t.Fatalf("transparentExchange() error = %v", err)
	}
	if !equalBytes(got, []byte{0xAA, 0xAA}) {
		t.Fatalf("transparentExchange() = %X, want AAAA", got)
	}
	if len(card.commands) != 7 {
		t.Fatalf("transparentExchange() sent %d commands, want 7", len(card.commands))
	}
	if card.commands[0][len(card.commands[0])-1] != 0x00 {
		t.Fatalf("first APDU = %X, want trailing Le", card.commands[0])
	}
	if card.commands[1][len(card.commands[1])-1] == 0x00 {
		t.Fatalf("second APDU = %X, want no trailing Le", card.commands[1])
	}
	if !equalBytes(card.commands[4][:6], []byte{0xFF, 0xC2, 0x00, 0x01, 0x0F, 0x90}) {
		t.Fatalf("fifth APDU prefix = %X, want short transparent APDU prefix", card.commands[4][:6])
	}
	if !equalBytes(card.commands[6][:7], []byte{0xFF, 0xC2, 0x00, 0x01, 0x00, 0x00, 0x0F}) {
		t.Fatalf("seventh APDU prefix = %X, want extended APDU prefix", card.commands[6][:7])
	}
}

func TestReaderEscapeControlCode(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0x12, 0x04, 0x42, 0x00, 0x00, 0x01, 0x13, 0x04, 0x42, 0x00, 0x00, 0x02},
	}}

	code, err := readerEscapeControlCode(context.Background(), card)
	if err != nil {
		t.Fatalf("readerEscapeControlCode() error = %v", err)
	}
	if code != 0x42000002 {
		t.Fatalf("readerEscapeControlCode() = 0x%08X, want 0x42000002", code)
	}
	if len(card.commands) != 1 || len(card.commands[0]) != 0 {
		t.Fatalf("readerEscapeControlCode() sent %d commands with first len=%d, want one empty command", len(card.commands), len(card.commands[0]))
	}
}

func TestReaderEscapeControlCodesIncludesByteSwappedVariant(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0x13, 0x04, 0x42, 0x00, 0x00, 0x02},
	}}

	codes, err := readerEscapeControlCodes(context.Background(), card)
	if err != nil {
		t.Fatalf("readerEscapeControlCodes() error = %v", err)
	}
	if len(codes) != 2 {
		t.Fatalf("readerEscapeControlCodes() returned %d codes, want 2", len(codes))
	}
	if codes[0] != 0x42000002 {
		t.Fatalf("readerEscapeControlCodes()[0] = 0x%08X, want 0x42000002", codes[0])
	}
	if codes[1] != 0x02000042 {
		t.Fatalf("readerEscapeControlCodes()[1] = 0x%08X, want 0x02000042", codes[1])
	}
}
