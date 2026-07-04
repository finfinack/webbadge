//go:build !darwin || !cgo

package pcsc

import (
	"context"
	"errors"
	"time"
)

// Session is unavailable on unsupported platforms.
type Session struct{}

// Card is unavailable on unsupported platforms.
type Card struct{}

// Establish reports that this build lacks PC/SC support.
func Establish() (*Session, error) {
	return nil, errors.New("PC/SC requires macOS with cgo enabled")
}

// Close is a no-op on unsupported platforms.
func (session *Session) Close() {}

// Readers reports unsupported PC/SC.
func (session *Session) Readers() ([]string, error) {
	return nil, errors.New("PC/SC requires macOS with cgo enabled")
}

// SelectReader reports unsupported PC/SC.
func (session *Session) SelectReader(string) (string, error) {
	return "", errors.New("PC/SC requires macOS with cgo enabled")
}

// WaitForCard reports unsupported PC/SC.
func (session *Session) WaitForCard(context.Context, string, time.Duration) (*Card, error) {
	return nil, errors.New("PC/SC requires macOS with cgo enabled")
}

// Close is a no-op on unsupported platforms.
func (card *Card) Close() {}

// Reader returns an empty reader name on unsupported platforms.
func (card *Card) Reader() string { return "" }

// ATR returns no ATR on unsupported platforms.
func (card *Card) ATR() []byte { return nil }

func (card *Card) ControlLegacy(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("PC/SC is unsupported on this platform")
}

func (card *Card) Control(context.Context, uint32, []byte) ([]byte, error) {
	return nil, errors.New("PC/SC is unsupported on this platform")
}

// Transmit reports unsupported PC/SC.
func (card *Card) Transmit(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("PC/SC requires macOS with cgo enabled")
}
