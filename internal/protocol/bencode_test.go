package protocol

import (
	"runtime"
	"testing"
)

func TestUnmarshalBencodeDoesNotAllocateDeclaredLength(t *testing.T) {
	// 13 bytes that declare a 128 MiB dictionary key.
	hostile := []byte("d134217727:xx")

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	var v map[string]any
	if err := UnmarshalBencode(hostile, &v); err == nil {
		t.Fatal("truncated input decoded without error")
	}

	runtime.ReadMemStats(&after)

	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("decoding %d hostile bytes allocated %d MiB", len(hostile), got>>20)
	}
}

func TestUnmarshalBencode(t *testing.T) {
	var v struct {
		A string `bencode:"a"`
	}
	if err := UnmarshalBencode([]byte("d1:a5:helloe"), &v); err != nil || v.A != "hello" {
		t.Fatalf("got %q, %v", v.A, err)
	}

	if err := UnmarshalBencode([]byte("d1:a5:helloetrailing"), &v); err == nil {
		t.Fatal("trailing bytes decoded without error")
	}

	if err := UnmarshalBencode(nil, &v); err == nil {
		t.Fatal("empty input decoded without error")
	}
}
