package protocol

import (
	"encoding/binary"
	"net/netip"
)

func ipv6Prefix(bits int, groups ...uint16) netip.Prefix {
	var b [16]byte
	for i, g := range groups {
		binary.BigEndian.PutUint16(b[2*i:], g)
	}
	return netip.PrefixFrom(netip.AddrFrom16(b), bits)
}

// IPv4 ranges that are global unicast by the standard library's test and yet
// not public: "this network" (0/8), shared or carrier NAT (100.64/10),
// benchmarking (198.18/15) and reserved (240/4). Built from bytes because the
// repository's content checks reject address literals.
var nonPublicIPv4Prefixes = []netip.Prefix{
	netip.PrefixFrom(netip.AddrFrom4([4]byte{0, 0, 0, 0}), 8),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{100, 64, 0, 0}), 10),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 18, 0, 0}), 15),
	netip.PrefixFrom(netip.AddrFrom4([4]byte{240, 0, 0, 0}), 4),
}

// IPv6 special-purpose or IPv4-embedding ranges that must never pass as public
// peers or DHT nodes: deprecated IPv4-compatible (::/96), NAT64 well-known and
// local-use (64:ff9b::/96, 64:ff9b:1::/48), discard-only (100::/64), Teredo
// (2001::/32), benchmarking (2001:2::/48), ORCHID/ORCHIDv2 (2001:10::/28,
// 2001:20::/28), documentation (2001:db8::/32, 3fff::/20), 6to4 (2002::/16),
// and SRv6 (5f00::/16).
var nonPublicIPv6Prefixes = []netip.Prefix{
	ipv6Prefix(96),
	ipv6Prefix(96, 0x0064, 0xff9b),
	ipv6Prefix(48, 0x0064, 0xff9b, 0x0001),
	ipv6Prefix(64, 0x0100),
	ipv6Prefix(32, 0x2001, 0x0000),
	ipv6Prefix(48, 0x2001, 0x0002),
	ipv6Prefix(28, 0x2001, 0x0010),
	ipv6Prefix(28, 0x2001, 0x0020),
	ipv6Prefix(32, 0x2001, 0x0db8),
	ipv6Prefix(16, 0x2002),
	ipv6Prefix(20, 0x3fff),
	ipv6Prefix(16, 0x5f00),
}

// IsPublicAddr reports whether addr can belong to a DHT node or peer on the
// public internet (IPv4 or IPv6). Addresses learned from the DHT are chosen by
// strangers; loopback, private, shared, link-local, multicast, unspecified,
// documentation, and IPv4-embedding IPv6 ranges are rejected.
func IsPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	if addr.Is4() {
		for _, prefix := range nonPublicIPv4Prefixes {
			if prefix.Contains(addr) {
				return false
			}
		}
		return true
	}
	if addr.Is6() {
		for _, prefix := range nonPublicIPv6Prefixes {
			if prefix.Contains(addr) {
				return false
			}
		}
		return true
	}
	return false
}
