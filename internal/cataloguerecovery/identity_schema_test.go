package cataloguerecovery

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIdentitySchemaCompatibilityIsLimitedToMigration70(t *testing.T) {
	old := []snapshotColumn{{Table: "torrents", Column: "info_hash", Type: "bytea", NotNull: true, Constraints: []string{"PRIMARY KEY (info_hash)"}}}
	before, err := json.Marshal(old)
	require.NoError(t, err)
	current := append([]snapshotColumn{}, old...)
	current[0].Constraints = append(append([]string{}, old[0].Constraints...), fullIdentityCheck, v2IdentityCheck)
	current = append(current, snapshotColumn{Table: "torrents", Column: "info_hash_v2", Type: "bytea", Constraints: current[0].Constraints})
	marshal := func(cols []snapshotColumn) []byte { b, e := json.Marshal(cols); require.NoError(t, e); return b }
	require.True(t, compatibleIdentitySchema(before, marshal(current)))
	current[1].NotNull = true
	require.False(t, compatibleIdentitySchema(before, marshal(current)))
	current[1].NotNull = false
	current[0].Type = "text"
	require.False(t, compatibleIdentitySchema(before, marshal(current)))
	current[0].Type = "bytea"
	current[0].Constraints = append(current[0].Constraints, "CHECK (true)")
	require.False(t, compatibleIdentitySchema(before, marshal(current)))
	require.False(t, compatibleIdentitySchema([]byte(`[]`), []byte(`[{"unknown":true}]`)))

	clock := "clock_timestamp()"
	for name, constraints := range map[string][]string{"pg16": bundleConstraintsPG16, "pg18": bundleConstraintsPG18} {
		t.Run("migration81_"+name, func(t *testing.T) {
			base70 := []snapshotColumn{
				{Table: "torrents", Column: "info_hash", Type: "bytea", NotNull: true, Constraints: []string{"PRIMARY KEY (info_hash)", fullIdentityCheck, v2IdentityCheck}},
				{Table: "torrents", Column: "info_hash_v2", Type: "bytea", Constraints: []string{"PRIMARY KEY (info_hash)", fullIdentityCheck, v2IdentityCheck}},
			}
			bundles := []snapshotColumn{
				{Table: "torrent_file_bundles", Column: "info_hash", Type: "bytea", NotNull: true, Constraints: constraints},
				{Table: "torrent_file_bundles", Column: "indexes", Type: "integer[]", NotNull: true, Constraints: constraints},
				{Table: "torrent_file_bundles", Column: "paths", Type: "text[]", NotNull: true, Constraints: constraints},
				{Table: "torrent_file_bundles", Column: "sizes", Type: "bigint[]", NotNull: true, Constraints: constraints},
				{Table: "torrent_file_bundles", Column: "extensions", Type: "text[]", NotNull: true, Constraints: constraints},
				{Table: "torrent_file_bundles", Column: "created_at", Type: "timestamp with time zone", NotNull: true, Default: &clock, Constraints: constraints},
				{Table: "torrent_file_bundles", Column: "updated_at", Type: "timestamp with time zone", NotNull: true, Default: &clock, Constraints: constraints},
			}
			with81 := append(append([]snapshotColumn{}, base70...), bundles...)
			// Pre-70 -> post-81, post-70 -> post-81, and post-81 -> pre-81 are all exact.
			require.True(t, compatibleIdentitySchema(before, marshal(with81)))
			require.True(t, compatibleIdentitySchema(marshal(base70), marshal(with81)))
			require.True(t, compatibleIdentitySchema(marshal(with81), marshal(base70)))
			// Any tampered torrent_file_bundles column or constraint fails closed.
			tampered := append([]snapshotColumn{}, with81...)
			tampered[len(tampered)-1].NotNull = false
			require.False(t, compatibleIdentitySchema(marshal(base70), marshal(tampered)))
			require.False(t, compatibleIdentitySchema(marshal(tampered), marshal(base70)))
		})
	}
}
