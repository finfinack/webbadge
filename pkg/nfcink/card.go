// Package nfcink implements the nfcink 240x416 four-colour NFC badge
// protocol recovered from the Python reference driver in old/nfcink.
package nfcink

import "context"

// Card is the minimum PC/SC card contract used by this protocol.
type Card interface {
	Transmit(ctx context.Context, command []byte) ([]byte, error)
}
