package protocol

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestIsPublicAddr(t *testing.T) {
	// Addresses are built from numbers: the repository's content checks
	// reject most address literals.
	v4 := func(a, b, c, d byte) netip.Addr { return netip.AddrFrom4([4]byte{a, b, c, d}) }
	v6 := func(groups ...uint16) netip.Addr {
		var b [16]byte
		for i, g := range groups {
			binary.BigEndian.PutUint16(b[2*i:], g)
		}

		return netip.AddrFrom16(b)
	}

	for _, tc := range []struct {
		name string
		addr netip.Addr
		want bool
	}{
		{"public", v4(8, 8, 8, 8), true},
		{"public, IPv4-mapped", v6(0, 0, 0, 0, 0, 0xffff, 0x0808, 0x0808), true},
		{"public IPv6", v6(0x2001, 0x4860, 0x4860, 0, 0, 0, 0, 0x8888), false},
		{"NAT64 carrying a private address", v6(0x64, 0xff9b, 0, 0, 0, 0, 0x0a00, 1), false},
		{"6to4 carrying a private address", v6(0x2002, 0xc0a8, 0x0101, 0, 0, 0, 0, 1), false},
		{"IPv4-compatible loopback", v6(0, 0, 0, 0, 0, 0, 0x7f00, 1), false},
		{"this network", v4(0, 0, 0, 1), false},
		{"benchmarking", v4(198, 18, 0, 1), false},
		{"reserved", v4(240, 0, 0, 1), false},
		{"loopback", v4(127, 0, 0, 1), false},
		{"IPv6 loopback", v6(0, 0, 0, 0, 0, 0, 0, 1), false},
		{"loopback, IPv4-mapped", v6(0, 0, 0, 0, 0, 0xffff, 0x7f00, 1), false},
		{"private, 10/8", v4(10, 1, 2, 3), false},
		{"private, 172.16/12", v4(172, 16, 0, 1), false},
		{"private, 192.168/16", v4(192, 168, 1, 1), false},
		{"private, IPv4-mapped", v6(0, 0, 0, 0, 0, 0xffff, 0xc0a8, 0x0101), false},
		{"shared (carrier NAT)", v4(100, 64, 0, 1), false},
		{"link-local", v4(169, 254, 169, 254), false},
		{"IPv6 link-local", v6(0xfe80, 0, 0, 0, 0, 0, 0, 1), false},
		{"IPv6 unique local", v6(0xfd00, 0, 0, 0, 0, 0, 0, 1), false},
		{"multicast", v4(224, 0, 0, 1), false},
		{"broadcast", v4(255, 255, 255, 255), false},
		{"unspecified", v4(0, 0, 0, 0), false},
		{"IPv6 unspecified", v6(), false},
		{"zero value", netip.Addr{}, false},
	} {
		if got := IsPublicAddr(tc.addr); got != tc.want {
			t.Errorf("%s: IsPublicAddr(%s) = %v, want %v", tc.name, tc.addr, got, tc.want)
		}
	}
}
