package metainforequester

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// allocatedBy reports the bytes allocated while f runs. Tests using it are not
// parallel, so nothing else in the package allocates meanwhile.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

type readWriter struct {
	*bytes.Reader
	bytes.Buffer
}

func (rw *readWriter) Read(p []byte) (int, error) { return rw.Reader.Read(p) }

// A peer chooses the length prefix of every message. The requester must not
// reserve that much before the bytes arrive, nor keep what it has no use for.
func TestPeerMessageLengthIsNotTrusted(t *testing.T) {
	t.Run("declared length with no payload", func(t *testing.T) {
		var err error

		allocated := allocatedBy(func() {
			_, err = readMessage(bytes.NewReader(framePrefix(maxMetadataSize)))
		})
		require.Error(t, err)
		require.Less(t, allocated, uint64(1<<20), "4 bytes from a peer allocated %d bytes", allocated)
	})

	t.Run("oversized message is skipped, not buffered", func(t *testing.T) {
		handshake := []byte{20, 0, 'd', 'e'}
		wire := append(frame(make([]byte, 4<<20)), frame(handshake)...)
		reader := bytes.NewReader(wire)

		var got []byte

		var err error

		allocated := allocatedBy(func() {
			got, err = readExMessage(reader)
		})
		require.NoError(t, err)
		require.Equal(t, handshake, got)
		require.Less(t, allocated, uint64(1<<20), "a 4 MiB message allocated %d bytes", allocated)
	})

	t.Run("declared metadata size with no pieces", func(t *testing.T) {
		var err error

		allocated := allocatedBy(func() {
			_, err = readAllPieces(bytes.NewReader(nil), maxMetadataSize-1)
		})
		require.Error(t, err)
		require.Less(t, allocated, uint64(1<<20), "a declared size allocated %d bytes", allocated)
	})

	t.Run("largest metadata piece still fits", func(t *testing.T) {
		metadata := patternedBytes(metadataPieceSize)

		got, err := readAllPieces(bytes.NewReader(metadataWire(metadata, 0)), uint(len(metadata)))
		require.NoError(t, err)
		require.Equal(t, metadata, got)
	})
}

// 12 bytes of bencode can declare a 128 MiB string.
func TestPeerBencodeLengthIsNotTrusted(t *testing.T) {
	hostile := []byte("d134217727:x")

	t.Run("extension handshake", func(t *testing.T) {
		rw := &readWriter{Reader: bytes.NewReader(frame(append([]byte{20, 0}, hostile...)))}

		var err error

		allocated := allocatedBy(func() {
			_, _, err = exHandshake(rw)
		})
		require.Error(t, err)
		require.Less(t, allocated, uint64(1<<20), "a %d-byte handshake allocated %d bytes", len(hostile), allocated)
	})

	t.Run("metadata piece header", func(t *testing.T) {
		wire := frame(append([]byte{20, 1}, hostile...))

		var err error

		allocated := allocatedBy(func() {
			_, err = readAllPieces(bytes.NewReader(wire), 1)
		})
		require.Error(t, err)
		require.Less(t, allocated, uint64(1<<20), "a %d-byte header allocated %d bytes", len(hostile), allocated)
	})
}

// Peer addresses are supplied by strangers, so by default the requester does
// not connect to the operator's own machine or network.
func TestRequesterRefusesNonPublicPeers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer listener.Close()

	accepted := make(chan struct{}, 2)

	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			accepted <- struct{}{}

			_ = conn.Close()
		}
	}()

	addr := listener.Addr().(*net.TCPAddr).AddrPort()
	request := func(config Config) error {
		config.RequestTimeout = 2 * time.Second
		r := New(Params{Config: config, Logger: zap.NewNop().Sugar()}).Requester
		_, requestErr := r.Request(context.Background(), protocol.RandomNodeID(), addr)

		return requestErr
	}

	require.ErrorContains(t, request(NewDefaultConfig()), "non-public address")

	select {
	case <-accepted:
		t.Fatal("the requester connected to a loopback address")
	case <-time.After(100 * time.Millisecond):
	}

	allowed := NewDefaultConfig()
	allowed.AllowPrivateAddrs = true
	require.Error(t, request(allowed)) // the listener hangs up before any handshake

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("AllowPrivateAddrs did not let the requester reach a loopback peer")
	}
}
