package protocol

import "net/netip"

// NodeIdentity is the output of the BEP-42 node ID pipeline: the 20-byte ID the
// DHT stack uses for IPv4 (and optionally a separate 20-byte IDv6 derived from
// the IPv6 external address when dual-stack IPv6 DHT is enabled), plus the
// resolved external IPs and fallback flags.
type NodeIdentity struct {
	ID               ID
	ExternalIP       netip.Addr
	RandomFallback   bool
	IDv6             ID
	ExternalIPv6     netip.Addr
	RandomFallbackV6 bool
}

// EffectiveIDv6 returns the IPv6-family 20-byte node ID, falling back to the
// primary IPv4 ID when no separate IPv6 identity was derived.
func (n NodeIdentity) EffectiveIDv6() ID {
	if n.IDv6 != (ID{}) {
		return n.IDv6
	}
	return n.ID
}

// IDForAddr selects the 20-byte BEP-42 node ID matching addr's IP family.
func (n NodeIdentity) IDForAddr(addr netip.Addr) ID {
	if addr.Unmap().Is6() {
		return n.EffectiveIDv6()
	}
	return n.ID
}
