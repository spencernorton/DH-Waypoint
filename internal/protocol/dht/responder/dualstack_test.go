package responder

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable/btree"
	"github.com/stretchr/testify/require"
)

type dualTableStub struct {
	ktable.Table
	orig4, orig6 protocol.ID
	nodes4       []ktable.Node
	nodes6       []ktable.Node
	hashPeers    []ktable.HashPeer
}

func (d dualTableStub) Origin() ktable.ID                         { return d.orig4 }
func (d dualTableStub) Origin6() ktable.ID                        { return d.orig6 }
func (d dualTableStub) GetClosestNodes(ktable.ID) []ktable.Node   { return d.nodes4 }
func (d dualTableStub) GetClosestNodes6(ktable.ID) []ktable.Node  { return d.nodes6 }
func (d dualTableStub) SampleNodes6(int) []ktable.Node            { return d.nodes6 }
func (d dualTableStub) PutNode(ktable.ID, netip.AddrPort, ...ktable.NodeOption) btree.PutResult {
	return btree.PutAccepted
}
func (d dualTableStub) GetHashOrClosestNodes(id ktable.ID) ktable.GetHashOrClosestNodesResult {
	if len(d.hashPeers) == 0 {
		return ktable.GetHashOrClosestNodesResult{ClosestNodes: d.nodes4}
	}
	return ktable.GetHashOrClosestNodesResult{
		Found: true,
		Hash:  stubHash{id: id, peers: d.hashPeers},
	}
}

type stubHash struct {
	id    protocol.ID
	peers []ktable.HashPeer
}

func (s stubHash) ID() protocol.ID          { return s.id }
func (s stubHash) Peers() []ktable.HashPeer { return s.peers }
func (s stubHash) Dropped() bool            { return false }

func v6AddrPort(port uint16, groups ...uint16) netip.AddrPort {
	var b [16]byte
	for i, g := range groups {
		binary.BigEndian.PutUint16(b[2*i:], g)
	}
	return netip.AddrPortFrom(netip.AddrFrom16(b), port)
}

func TestResponderBEP32WantAndPerFamilyIdentity(t *testing.T) {
	id4, id6 := protocol.RandomNodeID(), protocol.RandomNodeID()
	n4 := dht.NodeInfo{ID: protocol.RandomNodeID(), Addr: dht.NewNodeAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom4([4]byte{8, 8, 8, 8}), 6881))}
	n6 := dht.NodeInfo{ID: protocol.RandomNodeID(), Addr: dht.NewNodeAddrFromAddrPort(v6AddrPort(6881, 0x2606, 0x4700, 0x4700, 0, 0, 0, 0, 0x1111))}

	tbl := dualTableStub{
		orig4:  id4,
		orig6:  id6,
		nodes4: []ktable.Node{mockedPeer{n4}},
		nodes6: []ktable.Node{mockedPeer{n6}},
		hashPeers: []ktable.HashPeer{
			{Addr: n4.Addr.ToAddrPort()},
			{Addr: n6.Addr.ToAddrPort()},
		},
	}
	r := responder{nodeID: id4, nodeIDv6: id6, kTable: tbl, tokenSecret: []byte("secret"), sampleInfoHashesInterval: 10}
	target := protocol.RandomNodeID()
	from4 := netip.AddrPortFrom(netip.AddrFrom4([4]byte{1, 1, 1, 1}), 6881)
	from6 := v6AddrPort(6881, 0x2001, 0x4860, 0x4860, 0, 0, 0, 0, 0x8888)

	// 1. Default want from IPv6 sender returns IPv6 ID + nodes6 only.
	ret, err := r.Respond(context.Background(), dht.RecvMsg{
		From: from6,
		Msg:  dht.Msg{Q: dht.QFindNode, A: &dht.MsgArgs{ID: protocol.RandomNodeID(), Target: target}},
	})
	require.NoError(t, err)
	require.Equal(t, id6, ret.ID)
	require.Empty(t, ret.Nodes)
	require.Len(t, ret.Nodes6, 1)
	require.Equal(t, n6.ID, ret.Nodes6[0].ID)

	// 2. Explicit want=["n4","n6"] from IPv4 sender returns IPv4 ID + both nodes and nodes6.
	ret, err = r.Respond(context.Background(), dht.RecvMsg{
		From: from4,
		Msg:  dht.Msg{Q: dht.QFindNode, A: &dht.MsgArgs{ID: protocol.RandomNodeID(), Target: target, Want: []dht.Want{dht.WantNodes, dht.WantNodes6}}},
	})
	require.NoError(t, err)
	require.Equal(t, id4, ret.ID)
	require.Len(t, ret.Nodes, 1)
	require.Len(t, ret.Nodes6, 1)

	// 3. get_peers with want=["n6"] filters compact values strictly to 18-byte IPv6 peers.
	ret, err = r.Respond(context.Background(), dht.RecvMsg{
		From: from4,
		Msg:  dht.Msg{Q: dht.QGetPeers, A: &dht.MsgArgs{ID: protocol.RandomNodeID(), InfoHash: target, Want: []dht.Want{dht.WantNodes6}}},
	})
	require.NoError(t, err)
	require.Len(t, ret.Values, 1)
	require.True(t, ret.Values[0].ToAddrPort().Addr().Is6())
}
