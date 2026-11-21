package dhtcrawler

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveBootstrapAddrsFamilyFiltering(t *testing.T) {
	v4Lit := netip.AddrPortFrom(netip.AddrFrom4([4]byte{8, 8, 8, 8}), 6881).String()
	var b [16]byte
	for i, g := range []uint16{0x2606, 0x4700, 0x4700, 0, 0, 0, 0, 0x1111} {
		binary.BigEndian.PutUint16(b[2*i:], g)
	}
	v6Lit := netip.AddrPortFrom(netip.AddrFrom16(b), 6881).String()

	addrs4, err := resolveBootstrapAddrs(context.Background(), v4Lit, false)
	require.NoError(t, err)
	require.Len(t, addrs4, 1)
	require.True(t, addrs4[0].Addr().Is4())

	// IPv6 literal skipped cleanly when IPv6 DHT is disabled.
	skipped6, err := resolveBootstrapAddrs(context.Background(), v6Lit, false)
	require.NoError(t, err)
	require.Empty(t, skipped6)

	// IPv6 literal admitted when IPv6 DHT is enabled.
	addrs6, err := resolveBootstrapAddrs(context.Background(), v6Lit, true)
	require.NoError(t, err)
	require.Len(t, addrs6, 1)
	require.True(t, addrs6[0].Addr().Is6())
}
