package metainforequester

import (
	"bufio"
	"context"
	"crypto/sha1"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/mse"
	"github.com/anacrolix/torrent/peer_protocol"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/peerrep"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func startMSERequiredMetadataPeer(t *testing.T, infoBytes []byte, infoHash protocol.ID) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				// Peek first byte: plaintext BitTorrent starts with 0x13 ("\x13BitTorrent protocol").
				// Reject plaintext peers immediately like an encryption-required BitTorrent client.
				var first [1]byte
				if _, rErr := io.ReadFull(conn, first[:]); rErr != nil {
					return
				}
				if first[0] == 0x13 {
					return
				}
				// Reconstruct full stream including first byte for MSE ReceiveHandshake.
				prefixed := &prefixedConn{Conn: conn, first: first[:]}
				rw, _, hsErr := mse.ReceiveHandshake(context.Background(), prefixed, func(f func([]byte) bool) {
					f(infoHash[:])
				}, mse.DefaultCryptoSelector)
				if hsErr != nil {
					return
				}
				serveSyntheticBEP9(rw, infoHash, infoBytes)
			}(c)
		}
	}()

	return ln.Addr().(*net.TCPAddr).AddrPort()
}

type prefixedConn struct {
	net.Conn
	first []byte
}

func (p *prefixedConn) Read(b []byte) (int, error) {
	if len(p.first) > 0 {
		n := copy(b, p.first)
		p.first = p.first[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

func serveSyntheticBEP9(rw io.ReadWriter, infoHash protocol.ID, infoBytes []byte) {
	var hs [68]byte
	if _, err := io.ReadFull(rw, hs[:]); err != nil {
		return
	}
	var resp [68]byte
	resp[0] = 19
	copy(resp[1:20], "BitTorrent protocol")
	resp[25] = 0x10 // BEP 10 LTEP
	copy(resp[28:48], infoHash[:])
	copy(resp[48:68], "DHW-MSE-TEST-PEER-01")
	if _, err := rw.Write(resp[:]); err != nil {
		return
	}

	// Send extended handshake advertising ut_metadata=1 and metadata_size.
	extPayload, err := bencode.Marshal(map[string]any{
		"m":             map[string]int{"ut_metadata": 1},
		"metadata_size": len(infoBytes),
	})
	if err != nil {
		return
	}
	msg := peer_protocol.Message{
		Type:            peer_protocol.Extended,
		ExtendedID:      peer_protocol.HandshakeExtendedID,
		ExtendedPayload: extPayload,
	}
	if _, err := rw.Write(msg.MustMarshalBinary()); err != nil {
		return
	}

	// Read client's extended handshake and metadata request(s).
	dec := peer_protocol.Decoder{R: bufio.NewReader(rw), MaxLength: 64 * 1024}
	for {
		var in peer_protocol.Message
		if err := dec.Decode(&in); err != nil {
			return
		}
		if in.Type != peer_protocol.Extended || in.ExtendedID == peer_protocol.HandshakeExtendedID {
			continue
		}
		var req struct {
			MsgType int `bencode:"msg_type"`
			Piece   int `bencode:"piece"`
		}
		if err := bencode.Unmarshal(in.ExtendedPayload, &req); err != nil || req.MsgType != 0 {
			continue
		}
		hdr, err := bencode.Marshal(map[string]any{
			"msg_type":   1,
			"piece":      req.Piece,
			"total_size": len(infoBytes),
		})
		if err != nil {
			return
		}
		reply := peer_protocol.Message{
			Type:            peer_protocol.Extended,
			ExtendedID:      1,
			ExtendedPayload: append(hdr, infoBytes...),
		}
		_, _ = rw.Write(reply.MustMarshalBinary())
		return
	}
}

func TestRequesterMSEFallback(t *testing.T) {
	piece := sha1.Sum([]byte("synthetic mse test payload"))
	infoBytes, err := bencode.Marshal(map[string]any{
		"name":         "Synthetic.MSE.Peer.Test.mkv",
		"piece length": int64(16384),
		"length":       int64(26),
		"pieces":       piece[:],
	})
	require.NoError(t, err)
	infoHash := protocol.ID(sha1.Sum(infoBytes))

	addr := startMSERequiredMetadataPeer(t, infoBytes, infoHash)

	// 1. Default (EnableMSE=false) must fail against an MSE-required peer.
	plainOnly := New(Params{
		Config:        Config{RequestTimeout: 3 * time.Second, AllowPrivateAddrs: true, EnableMSE: false},
		PeerRepConfig: peerrep.NewDefaultConfig(),
		Logger:        zap.NewNop().Sugar(),
	})
	_, err = plainOnly.Requester.Request(context.Background(), infoHash, addr)
	require.Error(t, err, "plaintext-only requester must fail on MSE-required peer")

	// 2. Opt-in EnableMSE=true must succeed over MSE/PE within the single budget.
	mseEnabled := New(Params{
		Config:        Config{RequestTimeout: 3 * time.Second, AllowPrivateAddrs: true, EnableMSE: true},
		PeerRepConfig: peerrep.NewDefaultConfig(),
		Logger:        zap.NewNop().Sugar(),
	})
	res, err := mseEnabled.Requester.Request(context.Background(), infoHash, addr)
	require.NoError(t, err, "MSE fallback requester must succeed on MSE-required peer")
	require.Equal(t, "Synthetic.MSE.Peer.Test.mkv", res.Info.Name)
	require.GreaterOrEqual(t, testutil.CollectAndCount(mseEnabled.RequestTransportTotal), 2)
}
