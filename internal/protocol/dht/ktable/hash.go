package ktable

import (
	"net/netip"
	"sync"
	"time"
)

const (
	// maxPeersPerHash bounds what one infohash can hold. Peers arrive from
	// unauthenticated replies and announces, so the table must not grow with them.
	maxPeersPerHash = 200
	// peerTTL is how long a stored peer is served before it must be seen again.
	peerTTL = 30 * time.Minute
)

type hashKeyspace struct {
	keyspace[[]HashPeer, HashOption, Hash, *hash]
}

type Hash interface {
	keyspaceItem
	Peers() []HashPeer
	Dropped() bool
}

type hash struct {
	id ID
	// mu guards peers and droppedReason: a Hash is read by callers after the
	// table lock is released, while commands keep writing it.
	mu           sync.Mutex
	peers        map[string]HashPeer
	discoveredAt time.Time
	// lastRequestedAt time.Time
	droppedReason error
	reverseMap    *reverseMap
}

type HashPeer struct {
	Addr   netip.AddrPort
	seenAt time.Time
}

type HashOption interface {
	apply(*hash)
}

var _ keyspaceItemPrivate[[]HashPeer, HashOption, Hash] = (*hash)(nil)

func (h *hash) update(peers []HashPeer) {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()

	for key, p := range h.peers {
		if now.Sub(p.seenAt) > peerTTL {
			delete(h.peers, key)
			h.reverseMap.dropHashForAddrs(h.id, p.Addr.Addr())
		}
	}

	for _, p := range peers {
		key := p.Addr.Addr().String()
		if _, known := h.peers[key]; !known && len(h.peers) >= maxPeersPerHash {
			continue
		}

		p.seenAt = now
		h.peers[key] = p
		h.reverseMap.putAddrHashes(p.Addr.Addr(), h.id)
	}
}

func (h *hash) apply(option HashOption) {
	h.mu.Lock()
	defer h.mu.Unlock()

	option.apply(h)
}

func (h *hash) drop(reason error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.droppedReason = reason
	for _, p := range h.peers {
		h.reverseMap.dropHashForAddrs(h.id, p.Addr.Addr())
	}
}

func (h *hash) public() Hash {
	return h
}

func (h *hash) ID() ID {
	return h.id
}

// Peers returns a copy of the unexpired peers, in no particular order.
func (h *hash) Peers() []HashPeer {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	peers := make([]HashPeer, 0, len(h.peers))

	for _, p := range h.peers {
		if now.Sub(p.seenAt) <= peerTTL {
			peers = append(peers, p)
		}
	}

	return peers
}

func (h *hash) Dropped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.droppedReason != nil
}
