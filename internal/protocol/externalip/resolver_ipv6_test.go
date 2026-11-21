package externalip

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func v6Addr(groups ...uint16) netip.Addr {
	var b [16]byte
	for i, g := range groups {
		binary.BigEndian.PutUint16(b[2*i:], g)
	}
	return netip.AddrFrom16(b)
}

func TestIPv6ExternalIPResolverValidationAndOverride(t *testing.T) {
	pub6 := v6Addr(0x2606, 0x4700, 0x4700, 0, 0, 0, 0, 0x1111)

	// Static valid IPv6 override succeeds and produces verifiable BEP-42 IPv6 node ID.
	r, err := New(Config{Override: pub6.String(), Family: FamilyIPv6}, zap.NewNop().Sugar())
	require.NoError(t, err)
	got, err := r.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, pub6, got)
	id6, err := protocol.SecureNodeID(got)
	require.NoError(t, err)
	require.True(t, protocol.VerifySecureNodeID(id6, got))

	// Reject non-public IPv6 and IPv4 addresses when FamilyIPv6 is selected.
	for _, bad := range []netip.Addr{
		netip.AddrFrom4([4]byte{8, 8, 8, 8}),
		v6Addr(0, 0, 0, 0, 0, 0, 0, 1),          // loopback
		v6Addr(0xfd00, 0, 0, 0, 0, 0, 0, 1),     // ULA
		v6Addr(0xfe80, 0, 0, 0, 0, 0, 0, 1),     // link-local
		v6Addr(0x2001, 0x0db8, 0, 0, 0, 0, 0, 1), // documentation
		v6Addr(0x2002, 0xc0a8, 0x0101, 0, 0, 0, 0, 1),
	} {
		_, err := New(Config{Override: bad.String(), Family: FamilyIPv6}, zap.NewNop().Sugar())
		require.ErrorIs(t, err, ErrInvalidAddress, "expected rejection for %s", bad)
	}

	// Multi-source resolver skips IPv4/ULA echo and returns first public IPv6 source.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(v6Addr(0xfd00, 0, 0, 0, 0, 0, 0, 1).String() + "\n"))
	}))
	t.Cleanup(badSrv.Close)
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(pub6.String() + "\n"))
	}))
	t.Cleanup(goodSrv.Close)

	mr, err := New(Config{Sources: []string{badSrv.URL, goodSrv.URL}, Family: FamilyIPv6}, zap.NewNop().Sugar())
	require.NoError(t, err)
	got, err = mr.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, pub6, got)
}
