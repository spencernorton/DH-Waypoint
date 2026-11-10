package csamblocklist

import (
	"context"
	"errors"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

// noOp is the zero-cost Manager for tests and narrow direct wiring. The
// factory never returns it: the live service remembers this instance's
// own matches even with no feed.
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
func (noOp) Observe(context.Context, string, ...protocol.InfoHash)     {}
func (noOp) RemoveStored(context.Context, string, model.Torrent) (Outcome, error) {
	return "", errors.New("csamblocklist: the no-op manager removes nothing")
}
func (noOp) Refresh(_ context.Context) error { return nil }
func (noOp) Enabled() bool                   { return false }
func (noOp) Snapshot() Snapshot {
	return Snapshot{
		PerFeed: map[string]FeedStatus{},
	}
}
