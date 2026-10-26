package metainforequester

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/stretchr/testify/require"
)

func localPeer(t *testing.T, network, address string, serve func(net.Conn) error) (netip.AddrPort, <-chan error) {
	t.Helper()
	ln, err := net.Listen(network, address)
	if err != nil && network == "tcp6" {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			err = serve(conn)
		}
		done <- err
	}()
	return ln.Addr().(*net.TCPAddr).AddrPort(), done
}

func TestRequesterFetchesMetadataOverBothAddressFamilies(t *testing.T) {
	info := metainfo.Info{Name: "synthetic-peer.txt", Length: 1, PieceLength: metadataPieceSize, Pieces: make([]byte, 20)}
	metadata, err := bencode.Marshal(info)
	require.NoError(t, err)
	hash := protocol.ID(sha1.Sum(metadata))
	peerID := protocol.ID{0x45}
	for _, tc := range []struct{ name, network, address string }{
		{"IPv4", "tcp4", "127.0.0.1:0"},
		{"IPv6", "tcp6", "[::1]:0"},
		{"IPv4 mapped", "tcp4", "127.0.0.1:0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, served := localPeer(t, tc.network, tc.address, func(conn net.Conn) error {
				hs := make([]byte, 68)
				if _, err := io.ReadFull(conn, hs); err != nil {
					return err
				}
				if !bytes.Equal(hs[28:48], hash[:]) {
					return fmt.Errorf("wrong wire hash")
				}
				copy(hs[48:], peerID[:])
				if _, err := conn.Write(hs); err != nil {
					return err
				}
				if _, err := readMessage(conn); err != nil {
					return err
				}
				root, err := bencode.Marshal(rootDict{M: mDict{UTMetadata: 7}, MetadataSize: len(metadata)})
				if err != nil {
					return err
				}
				frame := append(uintToBigEndian4(uint(len(root)+2)), 20, 0)
				if _, err := conn.Write(append(frame, root...)); err != nil {
					return err
				}
				request, err := readMessage(conn)
				if err != nil {
					return err
				}
				if len(request) < 2 || request[0] != 20 || request[1] != 7 {
					return fmt.Errorf("metadata request ignored negotiated extension ID")
				}
				_, err = conn.Write(dataFrame(0, len(metadata), metadata))
				return err
			})
			if tc.name == "IPv4 mapped" {
				addr = netip.AddrPortFrom(netip.MustParseAddr("::ffff:127.0.0.1"), addr.Port())
			}
			r := requester{clientID: protocol.ID{0x23}, timeout: 2 * time.Second, dialer: &net.Dialer{}}
			response, err := r.Request(context.Background(), hash, addr)
			require.NoError(t, err)
			require.NoError(t, <-served)
			require.Equal(t, info.Name, response.Info.Name)
			require.Equal(t, peerID, response.PeerID)
		})
	}
}

func TestRequesterCancellationInterruptsConnectedHandshake(t *testing.T) {
	ready := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	addr, _ := localPeer(t, "tcp4", "127.0.0.1:0", func(conn net.Conn) error {
		if _, err := io.ReadFull(conn, make([]byte, 68)); err != nil {
			return err
		}
		close(ready)
		<-release
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	r := requester{clientID: protocol.ID{0x23}, timeout: 5 * time.Second, dialer: &net.Dialer{}}
	go func() {
		_, err := r.Request(ctx, protocol.ID{0x11}, addr)
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("peer did not receive handshake")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled request remained blocked on the connected socket")
	}
}

func TestRequesterDeadlineStillBoundsStalledPeer(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	addr, _ := localPeer(t, "tcp4", "127.0.0.1:0", func(net.Conn) error { <-release; return nil })
	r := requester{clientID: protocol.ID{0x23}, timeout: 50 * time.Millisecond, dialer: &net.Dialer{}}
	start := time.Now()
	_, err := r.Request(context.Background(), protocol.ID{0x11}, addr)
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
}
