package client

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/server"
	"github.com/stretchr/testify/require"
)

type recordingServer struct {
	server.Server
	lastAddr netip.AddrPort
	lastArgs dht.MsgArgs
	reply    dht.RecvMsg
}

func (r *recordingServer) Query(_ context.Context, addr netip.AddrPort, _ string, args dht.MsgArgs) (dht.RecvMsg, error) {
	r.lastAddr = addr
	r.lastArgs = args
	return r.reply, nil
}

func v6ClientAddr(groups ...uint16) netip.Addr {
	var b [16]byte
	for i, g := range groups {
		binary.BigEndian.PutUint16(b[2*i:], g)
	}
	return netip.AddrFrom16(b)
}

func TestDualStackClientSendsPerFamilyIDAndParsesNodes6(t *testing.T) {
	id4, id6 := protocol.RandomNodeID(), protocol.RandomNodeID()
	pub4 := netip.AddrPortFrom(netip.AddrFrom4([4]byte{8, 8, 8, 8}), 6881)
	pub6 := netip.AddrPortFrom(v6ClientAddr(0x2606, 0x4700, 0x4700, 0, 0, 0, 0, 0x1111), 6881)
	ula6 := netip.AddrPortFrom(v6ClientAddr(0xfd00, 0, 0, 0, 0, 0, 0, 1), 6881)

	srv := &recordingServer{
		reply: dht.RecvMsg{
			From: pub6,
			Msg: dht.Msg{
				Y: dht.YResponse,
				R: &dht.Return{
					ID: protocol.RandomNodeID(),
					Nodes: dht.CompactIPv4NodeInfo{
						{ID: protocol.RandomNodeID(), Addr: dht.NewNodeAddrFromAddrPort(pub4)},
					},
					Nodes6: dht.CompactIPv6NodeInfo{
						{ID: protocol.RandomNodeID(), Addr: dht.NewNodeAddrFromAddrPort(ula6)}, // must be dropped
						{ID: protocol.RandomNodeID(), Addr: dht.NewNodeAddrFromAddrPort(pub6)},
					},
				},
			},
		},
	}

	adapter := serverAdapter{nodeID: id4, nodeIDv6: id6, enableIPv6: true, server: srv}
	res, err := adapter.FindNode(context.Background(), pub6, protocol.RandomNodeID())
	require.NoError(t, err)
	require.Equal(t, id6, srv.lastArgs.ID, "IPv6 query must send IPv6 BEP-42 node ID")
	require.Equal(t, []dht.Want{dht.WantNodes, dht.WantNodes6}, srv.lastArgs.Want)
	require.Len(t, res.Nodes, 2)
	require.Equal(t, pub4, res.Nodes[0].Addr)
	require.Equal(t, pub6, res.Nodes[1].Addr)

	// When enableIPv6=false, Nodes6 is ignored and Want is omitted.
	adapterV4 := serverAdapter{nodeID: id4, nodeIDv6: id6, enableIPv6: false, server: srv}
	resV4, err := adapterV4.FindNode(context.Background(), pub4, protocol.RandomNodeID())
	require.NoError(t, err)
	require.Equal(t, id4, srv.lastArgs.ID)
	require.Nil(t, srv.lastArgs.Want)
	require.Len(t, resV4.Nodes, 1)
	require.Equal(t, pub4, resV4.Nodes[0].Addr)
}
