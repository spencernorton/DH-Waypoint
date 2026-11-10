package blocking

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/bloom"
	"github.com/spencernorton/DH-Waypoint/internal/cataloguerecovery"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

func TestRecoveryOffRejectsBlockBeforeBufferOrDatabaseAccess(t *testing.T) {
	hash := protocol.InfoHash{1}
	m := &manager{buffer: map[protocol.InfoHash]struct{}{}}
	for _, flush := range []bool{false, true} {
		if err := m.Block(context.Background(), []protocol.InfoHash{hash}, "blocking", "synthetic", flush); !errors.Is(err, cataloguerecovery.ErrDisabled) {
			t.Fatalf("Block error = %v, want recovery disabled", err)
		}
		if len(m.buffer) != 0 || m.filter != nil {
			t.Fatal("held removal changed buffer or bloom")
		}
	}
}

func TestRecoveryOffRejectsLegacyBufferedFlushBeforeDatabaseAccess(t *testing.T) {
	hash := protocol.InfoHash{1}
	m := &manager{buffer: map[protocol.InfoHash]struct{}{hash: {}}}
	if err := m.Flush(context.Background()); !errors.Is(err, cataloguerecovery.ErrDisabled) {
		t.Fatalf("Flush error = %v, want recovery disabled", err)
	}
	if len(m.buffer) != 1 || m.filter != nil {
		t.Fatal("held flush changed buffer or bloom")
	}
}

func TestIgnoreFiltersWithoutRecoveryWhileBlockStaysHeld(t *testing.T) {
	ignored, other := protocol.InfoHash{1}, protocol.InfoHash{2}
	m := &manager{
		buffer: map[protocol.InfoHash]struct{}{}, filter: bloom.NewDefaultStableBloomFilter(),
		maxBufferSize: 1000, maxFlushWait: time.Hour, lastFlushedAt: time.Now(),
	}
	m.Ignore([]protocol.InfoHash{ignored})
	kept, err := m.Filter(context.Background(), []protocol.InfoHash{ignored, other})
	if err != nil || len(kept) != 1 || kept[0] != other {
		t.Fatalf("Filter = %v, %v; want only the hash that was not ignored", kept, err)
	}
	if err := m.Block(context.Background(), []protocol.InfoHash{other}, "blocking", "synthetic", false); !errors.Is(err, cataloguerecovery.ErrDisabled) {
		t.Fatalf("Block error = %v, want recovery disabled", err)
	}
	if len(m.buffer) != 0 {
		t.Fatal("ignoring a hash let a held removal into the buffer")
	}
}
