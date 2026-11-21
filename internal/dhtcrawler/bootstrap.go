package dhtcrawler

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/ktable"
)

func resolveBootstrapAddrs(ctx context.Context, strAddr string, includeIPv6 bool) ([]netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(strAddr); err == nil {
		clean := netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if clean.Addr().Is6() && !includeIPv6 {
			return nil, nil
		}
		return []netip.AddrPort{clean}, nil
	}
	host, portStr, err := net.SplitHostPort(strAddr)
	if err != nil {
		return nil, err
	}
	portNum, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, err
	}
	network := "ip4"
	if includeIPv6 {
		network = "ip"
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, network, host)
	if err != nil {
		return nil, err
	}
	seen := make(map[netip.AddrPort]struct{}, len(ips))
	out := make([]netip.AddrPort, 0, len(ips))
	for _, ip := range ips {
		clean := ip.Unmap()
		if clean.Is6() && !includeIPv6 {
			continue
		}
		ap := netip.AddrPortFrom(clean, uint16(portNum))
		if _, dup := seen[ap]; dup {
			continue
		}
		seen[ap] = struct{}{}
		out = append(out, ap)
	}
	return out, nil
}

func (c *crawler) reseedBootstrapNodes(ctx context.Context) {
	interval := time.Duration(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			for _, strAddr := range c.bootstrapNodes {
				addrs, err := resolveBootstrapAddrs(ctx, strAddr, c.enableIPv6)
				if err != nil {
					c.logger.Warnf("failed to resolve bootstrap node address %s: %s", strAddr, err)
					continue
				}
				for _, addr := range addrs {
					select {
					case <-ctx.Done():
						return
					case c.nodesForPing.In() <- ktable.NewNode(ktable.ID{}, addr):
					}
				}
			}
		}

		interval = c.reseedBootstrapNodesInterval
	}
}
