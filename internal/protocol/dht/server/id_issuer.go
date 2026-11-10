package server

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
)

type IDIssuer interface {
	Issue() string
}

// variantIDIssuer issues transaction IDs made of a counter, which keeps the
// IDs of open queries distinct, and random bytes, so that a host which can
// forge a source address still cannot guess the ID of a query it did not see.
type variantIDIssuer struct {
	mu   sync.Mutex
	buf  [binary.MaxVarintLen64 + 4]byte
	next uint64
}

func (i *variantIDIssuer) Issue() string {
	i.mu.Lock()
	n := binary.PutUvarint(i.buf[:], i.next)
	i.next++
	_, _ = rand.Read(i.buf[n : n+4]) // never fails (Go 1.24+)
	id := string(i.buf[:n+4])
	i.mu.Unlock()

	return id
}
