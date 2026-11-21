package server

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func testV6(groups ...uint16) netip.Addr {
	var b [16]byte
	for i, g := range groups {
		binary.BigEndian.PutUint16(b[2*i:], g)
	}
	return netip.AddrFrom16(b)
}

func TestDualStackServerRoutesByAddressFamilyAndRespectsDisable(t *testing.T) {
	sock4 := &fakeSocket{in: make(chan packet, 16), out: make(chan packet, 16), closed: make(chan struct{})}
	sock6 := &fakeSocket{in: make(chan packet, 16), out: make(chan packet, 16), closed: make(chan struct{})}
	s := &server{
		stopped:          make(chan struct{}),
		socket:           sock4,
		socket6:          sock6,
		queries:          make(map[string]pendingQuery),
		queryTimeout:     2 * time.Second,
		responder:        stubResponder{id: protocol.RandomNodeID()},
		responderTimeout: time.Second,
		idIssuer:         &variantIDIssuer{},
		logger:           zap.NewNop().Sugar(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.readSocket(ctx, sock4)
	go s.readSocket(ctx, sock6)
	t.Cleanup(func() {
		cancel()
		close(sock4.closed)
		close(sock6.closed)
	})

	peer4 := netip.AddrPortFrom(netip.AddrFrom4([4]byte{8, 8, 8, 8}), 6881)
	peer6 := netip.AddrPortFrom(testV6(0x2606, 0x4700, 0x4700, 0, 0, 0, 0, 0x1111), 6881)
	id4, id6 := protocol.RandomNodeID(), protocol.RandomNodeID()

	// IPv4 query goes to sock4
	go func() {
		p := <-sock4.out
		var q dht.Msg
		require.NoError(t, bencode.Unmarshal(p.data, &q))
		sock4.in <- packet{data: marshal(dht.Msg{T: q.T, Y: dht.YResponse, R: &dht.Return{ID: id4}}), addr: peer4}
	}()
	res4, err := s.Query(context.Background(), peer4, dht.QPing, dht.MsgArgs{ID: protocol.RandomNodeID()})
	require.NoError(t, err)
	require.Equal(t, id4, res4.Msg.R.ID)

	// IPv6 query goes to sock6
	go func() {
		p := <-sock6.out
		var q dht.Msg
		require.NoError(t, bencode.Unmarshal(p.data, &q))
		sock6.in <- packet{data: marshal(dht.Msg{T: q.T, Y: dht.YResponse, R: &dht.Return{ID: id6}}), addr: peer6}
	}()
	res6, err := s.Query(context.Background(), peer6, dht.QPing, dht.MsgArgs{ID: protocol.RandomNodeID()})
	require.NoError(t, err)
	require.Equal(t, id6, res6.Msg.R.ID)

	// Server without socket6 rejects IPv6 queries cleanly with ErrIPv6Disabled.
	v4Only, _ := newTestServer(t)
	_, err = v4Only.Query(context.Background(), peer6, dht.QPing, dht.MsgArgs{ID: protocol.RandomNodeID()})
	require.ErrorIs(t, err, ErrIPv6Disabled)
}

func TestOutgoingDHTPacketBound1024Bytes(t *testing.T) {
	// Response with 40 IPv6 nodes (38B each = 1520B) + 40 IPv4 nodes (26B each = 1040B) + 50 values
	// must be trimmed cleanly to <= MaxOutgoingPacketBytes while remaining valid bencode with ID & Token.
	tok := "deadbeef01234567"
	ret := &dht.Return{ID: protocol.RandomNodeID(), Token: &tok}
	for range 40 {
		ret.Nodes = append(ret.Nodes, dht.RandomNodeInfo(4))
		ret.Nodes6 = append(ret.Nodes6, dht.RandomNodeInfo(16))
	}
	for range 50 {
		ret.Values = append(ret.Values, dht.RandomNodeInfo(16).Addr)
	}

	raw, err := encodeBounded(dht.Msg{T: "ab", Y: dht.YResponse, R: ret})
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), MaxOutgoingPacketBytes)

	var decoded dht.Msg
	require.NoError(t, bencode.Unmarshal(raw, &decoded))
	require.NotNil(t, decoded.R)
	require.Equal(t, ret.ID, decoded.R.ID)
	require.NotNil(t, decoded.R.Token)
	require.Equal(t, tok, *decoded.R.Token)

	// Unbounded oversized query with no trimmable response fields returns ErrPacketTooLarge.
	oversized := dht.Msg{
		T: "ab",
		Y: dht.YQuery,
		Q: dht.QPing,
		A: &dht.MsgArgs{ID: protocol.RandomNodeID(), Token: string(make([]byte, 1200))},
	}
	_, err = encodeBounded(oversized)
	require.ErrorIs(t, err, ErrPacketTooLarge)
}

func TestDualStackSocketRealLoopbackStartStop(t *testing.T) {
	s4 := NewSocketForFamily(false)
	require.NoError(t, s4.Open(netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 0)))
	require.NoError(t, s4.Close())

	s6 := NewSocketForFamily(true)
	if err := s6.Open(netip.AddrPortFrom(testV6(0, 0, 0, 0, 0, 0, 0, 1), 0)); err != nil {
		t.Skipf("IPv6 loopback unavailable on host: %v", err)
	}
	require.NoError(t, s6.Close())
}
