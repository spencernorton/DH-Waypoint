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
	marshal := func() []byte { b, e := json.Marshal(current); require.NoError(t, e); return b }
	require.True(t, compatibleIdentitySchema(before, marshal()))
	current[1].NotNull = true
	require.False(t, compatibleIdentitySchema(before, marshal()))
	current[1].NotNull = false
	current[0].Type = "text"
	require.False(t, compatibleIdentitySchema(before, marshal()))
	current[0].Type = "bytea"
	current[0].Constraints = append(current[0].Constraints, "CHECK (true)")
	require.False(t, compatibleIdentitySchema(before, marshal()))
	require.False(t, compatibleIdentitySchema([]byte(`[]`), []byte(`[{"unknown":true}]`)))
}
