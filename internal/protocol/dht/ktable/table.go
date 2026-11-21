package ktable

import (
	"net/netip"
	"sync"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable/btree"
)

type ID = protocol.ID

type TableOrigin interface {
	Origin() ID
}

type TableCommand interface {
	PutNode(id ID, addr netip.AddrPort, options ...NodeOption) btree.PutResult
	DropNode(id ID, reason error) bool
	PutHash(id ID, peers []HashPeer, options ...HashOption) btree.PutResult
}

type TableQuery interface {
	GetClosestNodes(id ID) []Node
	GetOldestNodes(cutoff time.Time, n int) []Node
	GetNodesForSampleInfoHashes(n int) []Node
	FilterKnownAddrs(addrs []netip.Addr) []netip.Addr
	GetHashOrClosestNodes(id ID) GetHashOrClosestNodesResult
	// SampleHashesAndNodes returns a random sample of up to 8 hashes and nodes, and the total hashes count.
	SampleHashesAndNodes() SampleHashesAndNodesResult
	// SnapshotNodeAddrs returns up to `limit` in-routing-table
	// node addresses suitable for persisting as a warm-start
	// bootstrap list. Ordering is unspecified; callers MUST NOT
	// assume recency. The Kademlia invariant keeps stale nodes
	// out of the table (they get evicted on PING failure), so a
	// random sample of in-table addrs is a reasonable seed set.
	//
	// Does NOT include the node's own origin address.
	SnapshotNodeAddrs(limit int) []netip.AddrPort
}

type Table interface {
	TableOrigin
	TableCommand
	TableQuery
	BatchCommand(commands ...Command)
}

// IPv6Table exposes the separate BEP-32 IPv6 routing table maintained alongside
// the IPv4 routing table. Kept as a separate interface so existing Table mocks
// remain compatible while dual-stack callers query IPv6 neighbors directly.
type IPv6Table interface {
	Origin6() ID
	GetClosestNodes6(id ID) []Node
	SampleNodes6(n int) []Node
}

type GetHashOrClosestNodesResult struct {
	Hash         Hash
	ClosestNodes []Node
	Found        bool
}

type SampleHashesAndNodesResult struct {
	Hashes      []Hash
	Nodes       []Node
	TotalHashes int
}

type table struct {
	mutex   sync.RWMutex
	origin  ID
	origin6 ID
	nodesK  int
	hashesK int
	nodes   nodeKeyspace
	nodes6  nodeKeyspace
	hashes  hashKeyspace
	addrs   *reverseMap
}

func (t *table) Origin() ID {
	return t.origin
}

func (t *table) Origin6() ID {
	return t.origin6
}

func (t *table) GetClosestNodes6(id ID) []Node {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	return t.nodes6.getClosest(id)
}

func (t *table) SampleNodes6(n int) []Node {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	if n <= 0 {
		return nil
	}
	return t.nodes6.getRandom(n)
}

func (t *table) BatchCommand(commands ...Command) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	for _, command := range commands {
		command.exec(t)
	}
}

func (t *table) PutNode(id ID, addr netip.AddrPort, options ...NodeOption) btree.PutResult {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	return PutNode{
		ID:      id,
		Addr:    addr,
		Options: options,
	}.execReturn(t)
}

func (t *table) DropNode(id ID, reason error) bool {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	return DropNode{
		ID:     id,
		Reason: reason,
	}.execReturn(t)
}

func (t *table) PutHash(id ID, peers []HashPeer, options ...HashOption) btree.PutResult {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	return PutHash{
		ID:      id,
		Peers:   peers,
		Options: options,
	}.execReturn(t)
}

func (t *table) GetClosestNodes(id ID) []Node {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return GetClosestPeers{
		ID: id,
	}.execReturn(t)
}

func (t *table) GetOldestNodes(cutoff time.Time, n int) []Node {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return GetOldestPeers{
		Cutoff: cutoff,
		N:      n,
	}.execReturn(t)
}

func (t *table) GetNodesForSampleInfoHashes(n int) []Node {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return GetNodesForSampleInfoHashes{
		N: n,
	}.execReturn(t)
}

func (t *table) FilterKnownAddrs(addrs []netip.Addr) []netip.Addr {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return FilterKnownAddrs{
		Addrs: addrs,
	}.execReturn(t)
}

func (t *table) GetHashOrClosestNodes(id ID) GetHashOrClosestNodesResult {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return GetHashOrClosestNodes{
		ID: id,
	}.execReturn(t)
}

func (t *table) SampleHashesAndNodes() SampleHashesAndNodesResult {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	return SampleHashesAndNodes{}.execReturn(t)
}

func (t *table) SnapshotNodeAddrs(limit int) []netip.AddrPort {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	if limit <= 0 {
		return nil
	}

	nodes4 := t.nodes.getRandom(limit)
	nodes6 := t.nodes6.getRandom(limit)
	addrs := make([]netip.AddrPort, 0, min(limit, len(nodes4)+len(nodes6)))
	for i := 0; len(addrs) < limit && (i < len(nodes4) || i < len(nodes6)); i++ {
		if i < len(nodes4) && nodes4[i].Addr().IsValid() {
			addrs = append(addrs, nodes4[i].Addr())
		}
		if len(addrs) < limit && i < len(nodes6) && nodes6[i].Addr().IsValid() {
			addrs = append(addrs, nodes6[i].Addr())
		}
	}
	return addrs
}
