// Package nfcbadge implements the badge protocols recovered from the Android APK.
package nfcbadge

import (
	"context"
	"errors"
	"fmt"
)

// Card is the minimum PC/SC card contract used by the badge protocols.
type Card interface {
	Transmit(ctx context.Context, command []byte) ([]byte, error)
	ATR() []byte
	Reader() string
}

var errMalformedResponse = errors.New("malformed card response")

// StatusError reports a non-success ISO 7816 status word.
type StatusError struct {
	Status uint16
}

// Error implements error.
func (err *StatusError) Error() string {
	return fmt.Sprintf("card returned status %04X", err.Status)
}

func transmitOK(ctx context.Context, card Card, command []byte) ([]byte, error) {
	response, err := card.Transmit(ctx, command)
	if err != nil {
		return nil, err
	}
	return unwrapStatusResponse(response)
}
