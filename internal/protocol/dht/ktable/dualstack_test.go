package ktable

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

func v6AP(port uint16, groups ...uint16) netip.AddrPort {
	var b [16]byte
	for i, g := range groups {
		binary.BigEndian.PutUint16(b[2*i:], g)
	}
	return netip.AddrPortFrom(netip.AddrFrom16(b), port)
}

func TestDualStackKTableSeparatesIPv4AndIPv6RoutingTables(t *testing.T) {
	orig4 := protocol.RandomNodeID()
	orig6 := protocol.RandomNodeID()
	tbl := newDualTable(orig4, orig6, newPrometheusCollector("nodes"), newPrometheusCollector("hashes")).(*table)

	require.Equal(t, orig4, tbl.Origin())
	require.Equal(t, orig6, tbl.Origin6())

	id4 := protocol.RandomNodeID()
	id6 := protocol.RandomNodeID()
	addr4 := netip.AddrPortFrom(netip.AddrFrom4([4]byte{8, 8, 4, 4}), 6881)
	addr6 := v6AP(6881, 0x2606, 0x4700, 0x4700, 0, 0, 0, 0, 0x1111)

	tbl.PutNode(id4, addr4, NodeBep51Support(true))
	tbl.PutNode(id6, addr6, NodeBep51Support(true))

	closest4 := tbl.GetClosestNodes(id4)
	require.Len(t, closest4, 1)
	require.Equal(t, addr4, closest4[0].Addr())

	closest6 := tbl.GetClosestNodes6(id6)
	require.Len(t, closest6, 1)
	require.Equal(t, addr6, closest6[0].Addr())

	// Both families appear in maintenance queries and bootstrap snapshots.
	oldest := tbl.GetOldestNodes(time.Now().Add(time.Hour), 10)
	require.Len(t, oldest, 2)
	sampleCands := tbl.GetNodesForSampleInfoHashes(10)
	require.Len(t, sampleCands, 2)
	snap := tbl.SnapshotNodeAddrs(10)
	require.Len(t, snap, 2)

	// Dropping IPv6 node does not disturb IPv4 table.
	require.True(t, tbl.DropNode(id6, nil))
	require.Empty(t, tbl.GetClosestNodes6(id6))
	require.Len(t, tbl.GetClosestNodes(id4), 1)
}
