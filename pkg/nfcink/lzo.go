package nfcink

// CompressLZO1X encodes data as an LZO1X-1-compatible compressed block,
// for the F0D3 upload path (protocol.py's write_image_d3 / minilzo.compress).
//
// This is an independent, from-scratch implementation: the LZO1X wire
// format was understood by reading the Linux kernel's public-domain-in-
// spirit-but-GPL-licensed lzo1x_decompress_safe reference decompressor,
// then re-derived and re-expressed here as new encoding logic — no GPL
// source was copied or transliterated. To keep the encoder small and
// easy to verify without real hardware, it deliberately emits only the
// subset of LZO1X op-codes needed for small inputs (this protocol's
// blocks are a fixed 2000 bytes, well inside every threshold below):
//
//   - literal runs (any length, via the standard short/extended forms)
//   - the single "16KB-distance" back-reference form (length 3+, distance
//     up to 16384 -- more than enough for a 2000-byte block, so the other
//     match forms, which only exist to shave a byte or two off the
//     encoding of very close or very far matches, are never needed)
//   - the standard end-of-stream marker
func CompressLZO1X(data []byte) []byte {
	if len(data) < 4 {
		return compressTiny(data)
	}

	output := make([]byte, 0, len(data))
	matches := findMatches(data)

	pos := 0
	for _, match := range matches {
		if match.start > pos {
			output = appendLiteralRun(output, data[pos:match.start])
		}
		gapStart := match.start + match.length
		gapEnd := gapStart
		trailing := 0
		if trailing = nextGapLen(matches, match, len(data)); trailing > 3 {
			trailing = 0
		} else {
			gapEnd = gapStart + trailing
		}
		output = appendMatch(output, match.distance, match.length, data[gapStart:gapEnd])
		pos = gapEnd
	}
	if pos < len(data) {
		output = appendLiteralRun(output, data[pos:])
	}
	return appendEOF(output)
}

// nextGapLen returns how many literal bytes follow match before the next
// event (the following match's start, or the end of data), so the caller
// can decide whether to embed them as this match's trailing literal count
// (0-3) or fall back to a standalone literal-run token (4+).
func nextGapLen(matches []matchSpan, match matchSpan, dataLen int) int {
	end := match.start + match.length
	next := dataLen
	for _, candidate := range matches {
		if candidate.start >= end && candidate.start < next {
			next = candidate.start
		}
	}
	return next - end
}

// compressTiny handles inputs under 4 bytes using LZO1X's special first-
// instruction shortcut (byte 17+n encodes n literal bytes, n = 1..3),
// since the general literal-run token cannot encode fewer than 4 bytes.
// An empty input needs no literal token at all: the bare end-of-stream
// marker alone is byte-for-byte a valid, empty LZO1X stream (byte 17 by
// itself starts a match instruction whose distance resolves to zero,
// which is the format's own EOF signal -- n=0 would require emitting
// exactly that same byte, so the two cases collapse into one). Kept only
// for robustness; the fixed 2000-byte block size this protocol actually
// uses never reaches this path.
func compressTiny(data []byte) []byte {
	if len(data) == 0 {
		return appendEOF(nil)
	}
	output := make([]byte, 0, len(data)+4)
	output = append(output, byte(17+len(data)))
	output = append(output, data...)
	return appendEOF(output)
}

// appendLiteralRun encodes one standalone literal-run token. length must
// be >= 4 (the format has no shorter standalone form; runs of 0-3 must
// instead be embedded as a preceding match's trailing literal count).
func appendLiteralRun(output []byte, literal []byte) []byte {
	length := len(literal)
	if length < 4 {
		panic("nfcink: literal run shorter than 4 bytes")
	}
	if length <= 18 {
		output = append(output, byte(length-3))
	} else {
		output = append(output, 0)
		output = appendExtendedLength(output, length-18)
	}
	return append(output, literal...)
}

// appendMatch encodes one M3-form back-reference: 1 <= distance <= 16384,
// length >= 3, followed by 0-3 raw trailing literal bytes.
func appendMatch(output []byte, distance, length int, trailing []byte) []byte {
	if length < 3 {
		panic("nfcink: match shorter than 3 bytes")
	}
	if distance < 1 || distance > 16384 {
		panic("nfcink: match distance out of range")
	}
	if length-2 <= 31 {
		output = append(output, byte(0b0010_0000|(length-2)))
	} else {
		output = append(output, 0b0010_0000)
		output = appendExtendedLength(output, length-33)
	}
	packed := uint16((distance-1)<<2) | uint16(len(trailing))
	output = append(output, byte(packed), byte(packed>>8))
	return append(output, trailing...)
}

// appendExtendedLength encodes extra as LZO1X's zero-run continuation:
// extra = zeroBytesCount*255 + terminator, terminator in 1..255.
func appendExtendedLength(output []byte, extra int) []byte {
	zeroBytesCount := (extra - 1) / 255
	terminator := extra - zeroBytesCount*255
	for index := 0; index < zeroBytesCount; index++ {
		output = append(output, 0)
	}
	return append(output, byte(terminator))
}

// appendEOF appends the standard end-of-stream marker.
func appendEOF(output []byte) []byte {
	return append(output, 0x11, 0x00, 0x00)
}
