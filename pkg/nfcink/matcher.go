package nfcink

// matchSpan is one accepted back-reference: data[start:start+length] can be
// reproduced by copying length bytes from distance bytes earlier in the
// output.
type matchSpan struct {
	start    int
	length   int
	distance int
}

const (
	minMatchLength     = 3
	maxMatchDistance   = 16384 // M3 form's cap; always satisfied by our <=2000-byte blocks.
	firstMatchMinStart = 4     // the initial literal run has no shorter standalone encoding.
)

// findMatches runs a simple greedy, single-candidate hash-chain search
// (good enough for small, fixed-size protocol blocks; not an optimal
// parser) and returns non-overlapping matches in input order.
func findMatches(data []byte) []matchSpan {
	var spans []matchSpan
	lastSeen := make(map[uint32]int)

	pos := 0
	for pos+minMatchLength <= len(data) {
		if len(spans) == 0 && pos < firstMatchMinStart {
			if pos+minMatchLength <= len(data) {
				lastSeen[hash3(data, pos)] = pos
			}
			pos++
			continue
		}

		key := hash3(data, pos)
		if candidate, ok := lastSeen[key]; ok &&
			pos-candidate <= maxMatchDistance && equalRun3(data, candidate, pos) {
			length := extendMatch(data, candidate, pos)
			spans = append(spans, matchSpan{start: pos, length: length, distance: pos - candidate})
			lastSeen[key] = pos
			pos += length
			continue
		}
		lastSeen[key] = pos
		pos++
	}
	return spans
}

func hash3(data []byte, pos int) uint32 {
	value := uint32(data[pos]) | uint32(data[pos+1])<<8 | uint32(data[pos+2])<<16
	return (value * 2654435761) >> 8
}

func equalRun3(data []byte, a, b int) bool {
	return data[a] == data[b] && data[a+1] == data[b+1] && data[a+2] == data[b+2]
}

func extendMatch(data []byte, candidate, pos int) int {
	length := 3
	for pos+length < len(data) && data[candidate+length] == data[pos+length] {
		length++
	}
	return length
}
