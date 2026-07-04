package nfcink

import (
	"context"
	"errors"
	"testing"
)

type scriptedCard struct {
	responses [][]byte
	commands  [][]byte
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
	return response, nil
}

func TestRefreshSucceedsImmediately(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{{0x90, 0x00}}}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if len(card.commands) != 2 { // refresh init + one poll
		t.Fatalf("sent %d commands, want 2", len(card.commands))
	}
}

func TestRefreshAutonomousDrawSkipsPoll(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{{0x01, 0x90, 0x00}}}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if len(card.commands) != 1 {
		t.Fatalf("sent %d commands, want 1 (no poll expected)", len(card.commands))
	}
}

func TestRefreshEscalatesThroughColorScreen(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0x68, 0xC6},       // first refresh-init -> escalate
		{0x68, 0xC6},       // retried refresh-init -> escalate to alt
		{0x90, 0x00},       // refresh-alt succeeds
		{0x00, 0x90, 0x00}, // poll succeeds
	}}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if len(card.commands) != 4 {
		t.Fatalf("sent %d commands, want 4", len(card.commands))
	}
}

func TestRefreshRetriesOn6986(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0x69, 0x86}, // rejected, retry from start
		{0x90, 0x00}, // succeeds this time
		{0x00, 0x90, 0x00},
	}}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestRefreshFailsAfterRepeated6986(t *testing.T) {
	responses := make([][]byte, 0, max6986Retries+1)
	for i := 0; i <= max6986Retries; i++ {
		responses = append(responses, []byte{0x69, 0x86})
	}
	card := &scriptedCard{responses: responses}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err == nil {
		t.Fatal("Refresh() error = nil, want failure after repeated 6986")
	}
}

func TestRefreshHardwareErrorIsTerminal(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{{0x69, 0x8A}}}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err == nil {
		t.Fatal("Refresh() error = nil, want hardware error")
	}
}

func TestRefreshBusyRetryOnce(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0x68, 0xCA}, // busy
		{0x90, 0x00}, // retry succeeds (bare 9000, len==2)
	}}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestRefreshPollEscalatesOnColorScreen(t *testing.T) {
	card := &scriptedCard{responses: [][]byte{
		{0x90, 0x00},       // refresh-init ok
		{0x68, 0xC6},       // poll says escalate
		{0x90, 0x00},       // re-sent refresh-init ok (retriedOnce=false again)
		{0x00, 0x90, 0x00}, // poll ok
	}}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
}

func TestRefreshTransportErrorIsTerminal(t *testing.T) {
	card := &scriptedCard{err: errors.New("session dropped")}
	device := NewNfcInkDevice(card, nil)
	if err := device.Refresh(context.Background(), 0); err == nil {
		t.Fatal("Refresh() error = nil, want transport error surfaced")
	}
}

func TestWriteImageD2ReportsProgress(t *testing.T) {
	card := &scriptedCard{}
	var lastCompleted, lastTotal int
	device := NewNfcInkDevice(card, func(completed, total int) {
		lastCompleted, lastTotal = completed, total
	})
	data := make([]byte, 600) // 3 chunks of <=250
	if err := device.WriteImageD2(context.Background(), data, 0); err != nil {
		t.Fatalf("WriteImageD2() error = %v", err)
	}
	if lastCompleted != lastTotal || lastTotal != 3 {
		t.Fatalf("progress = %d/%d, want 3/3", lastCompleted, lastTotal)
	}
	if len(card.commands) != 3 {
		t.Fatalf("sent %d commands, want 3", len(card.commands))
	}
}

func TestWriteImageD3RoundTripsThroughRealCompression(t *testing.T) {
	card := &scriptedCard{}
	device := NewNfcInkDevice(card, nil)
	data := make([]byte, 2500) // spans two D3 blocks (2000 + 500)
	for index := range data {
		data[index] = byte(index % 4)
	}
	if err := device.WriteImageD3(context.Background(), data, 0); err != nil {
		t.Fatalf("WriteImageD3() error = %v", err)
	}
	if len(card.commands) == 0 {
		t.Fatal("WriteImageD3() sent no commands")
	}
}

func TestReadConfigDegradesWithoutSecondAPDU(t *testing.T) {
	a0Value := []byte{0x00, 0x00, 0x31, 0x03, 0x40, 0x00, 0xF0}
	resp1 := append([]byte{0xA0, byte(len(a0Value))}, a0Value...)
	resp1 = append(resp1, 0x90, 0x00)

	// The second APDU (device check) fails; ReadConfig should still
	// succeed using just the TLV response, matching read_config()'s
	// graceful degradation in the Python driver.
	device := NewNfcInkDevice(&sequencedCard{first: resp1, secondErr: errors.New("no PIN check")}, nil)
	cfg, err := device.ReadConfig(context.Background())
	if err != nil {
		t.Fatalf("ReadConfig() error = %v", err)
	}
	if cfg.ScreenWidth != 240 {
		t.Fatalf("ScreenWidth = %d, want 240", cfg.ScreenWidth)
	}
}

// sequencedCard returns `first` for the first Transmit call and an error
// for every subsequent call, to exercise ReadConfig's graceful degradation
// when the PIN/4-color-marker APDU fails.
type sequencedCard struct {
	first     []byte
	secondErr error
	calls     int
}

func (card *sequencedCard) Transmit(_ context.Context, _ []byte) ([]byte, error) {
	card.calls++
	if card.calls == 1 {
		return card.first, nil
	}
	return nil, card.secondErr
}
