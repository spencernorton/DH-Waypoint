package ktable

import (
	"net/netip"

	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable/btree"
)

type Command interface {
	exec(*table)
}

type CommandReturn[T any] interface {
	Command
	Query[T]
}

var _ CommandReturn[btree.PutResult] = PutNode{}

type PutNode struct {
	ID      ID
	Addr    netip.AddrPort
	Options []NodeOption
}

func (c PutNode) execReturn(t *table) btree.PutResult {
	if !c.Addr.IsValid() {
		return btree.PutRejected
	}
	clean := netip.AddrPortFrom(c.Addr.Addr().Unmap(), c.Addr.Port())
	if clean.Addr().Is6() {
		return t.nodes6.put(c.ID, clean, c.Options...)
	}
	return t.nodes.put(c.ID, clean, c.Options...)
}

func (c PutNode) exec(t *table) {
	c.execReturn(t)
}

var _ CommandReturn[bool] = DropNode{}

type DropNode struct {
	ID     ID
	Reason error
}

func (c DropNode) execReturn(t *table) bool {
	d4 := t.nodes.drop(c.ID, c.Reason)
	d6 := t.nodes6.drop(c.ID, c.Reason)
	return d4 || d6
}

func (c DropNode) exec(t *table) {
	c.execReturn(t)
}

var _ CommandReturn[bool] = DropAddr{}

type DropAddr struct {
	Addr   netip.Addr
	Reason error
}

func (c DropAddr) execReturn(t *table) bool {
	clean := c.Addr.Unmap()
	id, ok := t.addrs.getPeerIDForAddr(clean)
	if !ok {
		return false
	}
	if clean.Is6() {
		return t.nodes6.drop(id, c.Reason)
	}
	return t.nodes.drop(id, c.Reason)
}

func (c DropAddr) exec(t *table) {
	c.execReturn(t)
}

var _ CommandReturn[btree.PutResult] = PutHash{}

type PutHash struct {
	ID      ID
	Peers   []HashPeer
	Options []HashOption
}

func (c PutHash) execReturn(t *table) btree.PutResult {
	return t.hashes.put(c.ID, c.Peers, c.Options...)
}

func (c PutHash) exec(t *table) {
	c.execReturn(t)
}
