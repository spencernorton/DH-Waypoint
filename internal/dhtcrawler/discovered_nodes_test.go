package dhtcrawler

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/concurrency"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable"
	ktable_mocks "github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The sender of an incoming query, and every node a reply names, becomes a
// node the crawler queries. None of them may sit on the operator's network.
func TestDiscoveredNodesMustBePublic(t *testing.T) {
	t.Parallel()

	table := ktable_mocks.NewTable(t)
	table.On("FilterKnownAddrs", mock.Anything).Return(func(addrs []netip.Addr) []netip.Addr { return addrs })

	c := &crawler{
		kTable:           table,
		discoveredNodes:  concurrency.NewBatchingChannel[ktable.Node](10, 10, time.Millisecond),
		nodesForFindNode: concurrency.NewBufferedConcurrentChannel[ktable.Node](10, 1),
		// Unbuffered and unread, so every node goes to the channel above.
		nodesForSampleInfoHashes: concurrency.NewBufferedConcurrentChannel[ktable.Node](0, 1),
		nodesForPing:             concurrency.NewBufferedConcurrentChannel[ktable.Node](0, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.runDiscoveredNodes(ctx)

	passed := make(chan netip.AddrPort, 10)

	go func() {
		_ = c.nodesForFindNode.Run(ctx, func(n ktable.Node) { passed <- n.Addr() })
	}()

	// Nodes are handled in order, so anything let through arrives before the
	// public node sent last.
	public := netip.MustParseAddrPort("203.0.113.7:6881")
	for _, addr := range []netip.AddrPort{
		netip.MustParseAddrPort("127.0.0.1:5432"),
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 168, 1, 1}), 22), // a LAN address
		netip.MustParseAddrPort("[2001:db8::1]:6881"),
	} {
		c.discoveredNodes.In() <- ktable.NewNode(protocol.RandomNodeID(), addr)
	}

	c.discoveredNodes.In() <- ktable.NewNode(protocol.RandomNodeID(), public)

	select {
	case addr := <-passed:
		require.Equal(t, public, addr)
	case <-time.After(5 * time.Second):
		t.Fatal("the public node was not passed on")
	}
}
