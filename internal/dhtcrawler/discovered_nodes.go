package dhtcrawler

import (
	"context"
	"net/netip"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/concurrency"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable"
	"go.uber.org/fx"
	"golang.org/x/time/rate"
)

const (
	discoveredAddrDedupeMax = 8192
	discoveredAddrDedupeTTL = 30 * time.Second
)

type DiscoveredNodesParams struct {
	fx.In
	Config Config
}

type DiscoveredNodesResult struct {
	fx.Out
	DiscoveredNodes concurrency.BatchingChannel[ktable.Node] `name:"dht_discovered_nodes"`
}

// NewDiscoveredNodes creates the channel for discovered nodes.
// It receives nodes discovered by the crawler, as well as nodes from incoming requests to the DHT server.
// It is provided as a separate service to avoid a circular dependency with the DHT server.
func NewDiscoveredNodes(params DiscoveredNodesParams) DiscoveredNodesResult {
	return DiscoveredNodesResult{
		DiscoveredNodes: concurrency.NewBatchingChannel[ktable.Node](
			int(100*params.Config.ScalingFactor), 10, time.Second/100),
	}
}

type recentAddrFilter struct {
	seen map[netip.Addr]time.Time
	max  int
	ttl  time.Duration
}

func newRecentAddrFilter(maxEntries int, ttl time.Duration) *recentAddrFilter {
	return &recentAddrFilter{
		seen: make(map[netip.Addr]time.Time, min(maxEntries, 1024)),
		max:  maxEntries,
		ttl:  ttl,
	}
}

func (f *recentAddrFilter) allow(addr netip.Addr, now time.Time) bool {
	if exp, ok := f.seen[addr]; ok && now.Before(exp) {
		return false
	}
	if len(f.seen) >= f.max {
		for k, exp := range f.seen {
			if !now.Before(exp) {
				delete(f.seen, k)
			}
		}
		if len(f.seen) >= f.max {
			clear(f.seen)
		}
	}
	f.seen[addr] = now.Add(f.ttl)
	return true
}

func (c *crawler) runDiscoveredNodes(ctx context.Context) {
	recent := newRecentAddrFilter(discoveredAddrDedupeMax, discoveredAddrDedupeTTL)
	var limiter *rate.Limiter
	if c.maxDiscoveredNodesPerSecond > 0 {
		rps := rate.Limit(c.maxDiscoveredNodesPerSecond)
		burst := max(1, min(int(c.maxDiscoveredNodesPerSecond), 64))
		limiter = rate.NewLimiter(rps, burst)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ps := <-c.discoveredNodes.Out():
			now := time.Now()
			addrs := make([]netip.Addr, 0, len(ps))
			m := make(map[netip.Addr]ktable.Node, len(ps))
			for _, p := range ps {
				addr := p.Addr().Addr()
				// Every discovered node passes here, including the sender of
				// an incoming query. Only public addresses are queried.
				if !protocol.IsPublicAddr(addr) {
					continue
				}
				if _, ok := m[addr]; !ok {
					m[addr] = p
					addrs = append(addrs, addr)
				}
			}
			unknownAddrs := c.kTable.FilterKnownAddrs(addrs)
			for _, addr := range unknownAddrs {
				if !recent.allow(addr, now) {
					continue
				}
				if limiter != nil {
					if err := limiter.Wait(ctx); err != nil {
						return
					}
				}
				p := m[addr]
				select {
				case <-ctx.Done():
					return
				case c.nodesForFindNode.In() <- p:
				case c.nodesForSampleInfoHashes.In() <- p:
				case c.nodesForPing.In() <- p:
				}
			}
		}
	}
}
