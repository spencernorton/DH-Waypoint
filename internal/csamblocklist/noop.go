package csamblocklist

import (
	"context"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

// noOp is the zero-cost Manager. Returned by the factory when
// Config.Enabled=false or no feeds are configured.
//
// IsBlocked always returns false; Filter is a pass-through. The DHT
// crawler's hot path can call this at line rate without measurable
// overhead.
type noOp struct{}

// NewNoOp returns the zero-cost Manager.
func NewNoOp() Manager {
	return noOp{}
}

func (noOp) IsBlocked(_ protocol.InfoHash) bool                        { return false }
func (noOp) Filter(infoHashes []protocol.InfoHash) []protocol.InfoHash { return infoHashes }
func (noOp) Refresh(_ context.Context) error                           { return nil }
func (noOp) Enabled() bool                                             { return false }
func (noOp) Snapshot() Snapshot {
	return Snapshot{
		PerFeed: map[string]FeedStatus{},
	}
}
