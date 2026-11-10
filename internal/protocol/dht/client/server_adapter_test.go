package client

import (
	"net/netip"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht"
)

func publicAddr(i int) dht.NodeAddr {
	return dht.NodeAddr{IP: []byte{8, byte(i >> 16), byte(i >> 8), byte(i)}, Port: 6881}
}

// A reply is written by a stranger. Addresses in it that point at the
// operator's own machine or network must never be stored or contacted.
func TestReplyAddressesMustBePublic(t *testing.T) {
	hostile := []dht.NodeAddr{
		{IP: []byte{127, 0, 0, 1}, Port: 5432},
		{IP: []byte{192, 168, 1, 1}, Port: 22},
		{IP: []byte{10, 0, 0, 5}, Port: 6379},
		{IP: []byte{169, 254, 169, 254}, Port: 80},
		{IP: []byte{100, 64, 0, 1}, Port: 22},
		{IP: netip.MustParseAddr("::1").AsSlice(), Port: 5432},
		{IP: []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, Port: 22}, // IPv6 link-local
	}
	msg := dht.Msg{R: &dht.Return{Values: append(hostile, publicAddr(1))}}

	for _, addr := range hostile {
		msg.R.Nodes = append(msg.R.Nodes, dht.NodeInfo{ID: protocol.RandomNodeID(), Addr: addr})
	}

	msg.R.Nodes = append(msg.R.Nodes, dht.NodeInfo{ID: protocol.RandomNodeID(), Addr: publicAddr(2)})

	if got := extractValues(msg); len(got) != 1 || got[0] != publicAddr(1).ToAddrPort() {
		t.Fatalf("values: got %v, want only %v", got, publicAddr(1).ToAddrPort())
	}

	if got := extractNodes(msg); len(got) != 1 || got[0].Addr != publicAddr(2).ToAddrPort() {
		t.Fatalf("nodes: got %v, want only %v", got, publicAddr(2).ToAddrPort())
	}
}

// One datagram can carry about 8,000 peers or 3,000 samples.
func TestReplyContentsAreCapped(t *testing.T) {
	samples := make(dht.CompactInfohashes, 3000)
	msg := dht.Msg{R: &dht.Return{Bep51Return: dht.Bep51Return{Samples: &samples}}}

	for i := range 8000 {
		msg.R.Values = append(msg.R.Values, publicAddr(i))
		msg.R.Nodes = append(msg.R.Nodes, dht.NodeInfo{ID: protocol.RandomNodeID(), Addr: publicAddr(i)})
	}

	if got := len(extractValues(msg)); got != maxPerReply {
		t.Errorf("took %d values from one reply, want %d", got, maxPerReply)
	}

	if got := len(extractNodes(msg)); got != maxPerReply {
		t.Errorf("took %d nodes from one reply, want %d", got, maxPerReply)
	}

	if got := len(extractSamples(msg)); got != maxPerReply {
		t.Errorf("took %d samples from one reply, want %d", got, maxPerReply)
	}

	if got := extractSamples(dht.Msg{R: &dht.Return{}}); got != nil {
		t.Errorf("got %v for a reply without samples, want nil", got)
	}
}
