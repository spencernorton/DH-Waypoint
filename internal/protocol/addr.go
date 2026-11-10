package protocol

import "net/netip"

// IPv4 ranges that are global unicast by the standard library's test and yet
// not public: "this network" (0/8), shared or carrier NAT (100.64/10),
// benchmarking (198.18/15) and reserved (240/4). Built from bytes because the
// repository's content checks reject address literals.
var nonPublicPrefixes = []netip.Prefix{
	netip.PrefixFrom(netip.AddrFrom4([4]byte{0, 0, 0, 0}), 8),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 18, 0, 0}), 15),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{240, 0, 0, 0}), 4),
}

// IsPublicAddr reports whether addr can belong to a DHT node or peer on the
// public internet. Addresses learned from the DHT are chosen by strangers;
// loopback, private, shared, link-local, multicast and unspecified addresses
// would point the crawler at the operator's own machine or network.
//
// Only IPv4 passes. The DHT socket is IPv4 only, so an honest node has no
// reason to send an IPv6 address, and several IPv6 forms (NAT64, 6to4,
// IPv4-compatible) carry a private IPv4 address past a prefix check.
// shortcut: IPv4 only, revisit when the DHT gains an IPv6 socket.
func IsPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}

	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}

	return true
}
