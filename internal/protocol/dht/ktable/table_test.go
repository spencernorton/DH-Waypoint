package ktable

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

func newTestTable() *table {
	return newTable(protocol.RandomNodeID(), newPrometheusCollector("nodes"), newPrometheusCollector("hashes")).(*table)
}

func testPeer(i int) HashPeer {
	return HashPeer{Addr: netip.AddrPortFrom(netip.AddrFrom4([4]byte{203, byte(i >> 16), byte(i >> 8), byte(i)}), 6881)}
}

// A get_peers reply reads a hash's peers after the table lock is released,
// while announces keep writing them. Unsynchronised, the runtime aborts the
// process with "concurrent map iteration and map write".
func TestHashPeersReadWhileUpdated(t *testing.T) {
	tbl := newTestTable()
	id := tbl.origin
	id[19] ^= 1
	tbl.PutHash(id, []HashPeer{testPeer(0)})

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		for i := range 20_000 {
			tbl.PutHash(id, []HashPeer{testPeer(i % 150)})
		}
	}()

	for range 20_000 {
		res := tbl.GetHashOrClosestNodes(id)
		if !res.Found {
			t.Fatal("hash not found")
		}

		_ = res.Hash.Peers()
		_ = res.Hash.Dropped()
	}

	wg.Wait()
}

// The crawler keeps a Node from a query and reads it later, while replies
// keep updating the same node.
func TestNodeReadWhileUpdated(t *testing.T) {
	tbl := newTestTable()
	id := protocol.RandomNodeID()
	addr := netip.MustParseAddrPort("203.0.113.5:6881")
	tbl.PutNode(id, addr)

	nodes := tbl.GetOldestNodes(time.Now().Add(time.Hour), 1)
	if len(nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(nodes))
	}

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		for i := range 2_000 {
			tbl.PutNode(
				id,
				netip.AddrPortFrom(addr.Addr(), uint16(6881+i%2)),
				NodeResponded(),
				NodeBep51Support(true),
				NodeSampleInfoHashesRes(1, 1, time.Now()),
			)
		}

		tbl.DropNode(id, nil)
	}()

	n := nodes[0]
	for range 2_000 {
		_ = n.ID()
		_ = n.Addr()
		_ = n.Time()
		_ = n.Dropped()
		_ = n.IsSampleInfoHashesCandidate()
	}

	wg.Wait()
}

func TestHashPeersAreCappedAndExpire(t *testing.T) {
	tbl := newTestTable()
	id := tbl.origin
	id[19] ^= 1

	many := make([]HashPeer, 0, 5*maxPeersPerHash)
	for i := range 5 * maxPeersPerHash {
		many = append(many, testPeer(i))
	}

	tbl.PutHash(id, many)      // creates the hash
	tbl.PutHash(id, many[10:]) // updates it

	h := tbl.hashes.items[id]
	if got := len(h.Peers()); got != maxPeersPerHash {
		t.Fatalf("stored %d peers, want the cap of %d", got, maxPeersPerHash)
	}

	if got := tbl.addrs.len(); got != maxPeersPerHash {
		t.Fatalf("reverse map holds %d addresses, want %d", got, maxPeersPerHash)
	}

	// A known peer is refreshed even when the hash is full.
	known := h.Peers()[0]
	known.Addr = netip.AddrPortFrom(known.Addr.Addr(), 51413)
	tbl.PutHash(id, []HashPeer{known})

	if got := h.peers[known.Addr.Addr().String()].Addr.Port(); got != 51413 {
		t.Fatalf("known peer kept port %d, want the refreshed 51413", got)
	}

	for key, p := range h.peers {
		p.seenAt = time.Now().Add(-peerTTL - time.Second)
		h.peers[key] = p
	}

	if got := len(h.Peers()); got != 0 {
		t.Fatalf("served %d expired peers, want 0", got)
	}

	fresh := testPeer(1 << 20)
	tbl.PutHash(id, []HashPeer{fresh})

	if got := h.Peers(); len(got) != 1 || got[0].Addr != fresh.Addr {
		t.Fatalf("after expiry got %v, want only %v", got, fresh.Addr)
	}

	if got := tbl.addrs.len(); got != 1 {
		t.Fatalf("reverse map holds %d addresses after expiry, want 1", got)
	}
}
