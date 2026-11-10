package metainfo

import (
	"strconv"
	"strings"
	"testing"
)

// nestedPayload wraps a payload of the given size in depth dictionaries.
func nestedPayload(depth, size int) []byte {
	return []byte(strings.Repeat("d1:d", depth) + "d1:a" + strconv.Itoa(size) + ":" +
		strings.Repeat("x", size) + "e" + strings.Repeat("e", depth))
}

// The decoders that run after validation copy every container once per level
// of nesting. A deep chain around a large payload must be refused before then:
// 60 levels around 2 MiB would cost 120 MiB of copies.
func TestValidateEncodingBoundsNestingTimesSize(t *testing.T) {
	const payload = 2 << 20

	if err := validateEncoding(nestedPayload(60, payload)); err == nil {
		t.Fatal("a 2 MiB payload nested 60 levels deep passed validation")
	}

	if err := validateEncoding(nestedPayload(8, payload)); err != nil {
		t.Fatalf("a 2 MiB payload nested 8 levels deep was refused: %v", err)
	}

	// A flat dictionary of the largest accepted size costs one copy.
	if err := validateEncoding(nestedPayload(0, MaxMetaInfoBytes-64)); err != nil {
		t.Fatalf("a flat dictionary of the maximum size was refused: %v", err)
	}
}
