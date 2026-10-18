package processors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

// Dropping a file registers the plugin, editing it updates the
// registration, removing it deregisters — and registrations made
// through the operator API are never touched by directory sync.
func TestPluginDirSyncLifecycle(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := testContext
	dir := t.TempDir()
	s.cfg.PluginsDir = dir

	// An operator-API registration the sync must leave alone.
	_, err := s.Register(ctx, "api-owned", v1.RegistryWrite{
		Driver:        v1.DriverBuiltinFilter,
		Mode:          "shadow",
		State:         "active",
		Configuration: json.RawMessage(`{"reason":"api"}`),
	})
	require.NoError(t, err)

	filePath := filepath.Join(dir, "file-filter.plugin.yml")
	require.NoError(t, os.WriteFile(filePath, []byte(`
driver: builtin/filter-list
configuration: {reason: first}
`), 0o644))

	require.NoError(t, s.SyncPluginDir(ctx))
	entries, err := s.Entries(ctx)
	require.NoError(t, err)
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	require.Contains(t, byID, "file-filter")
	require.Contains(t, byID, "api-owned")
	firstGen := byID["file-filter"].Generation

	// A second sync with no changes must not re-register (generation stable).
	require.NoError(t, s.SyncPluginDir(ctx))
	entries, err = s.Entries(ctx)
	require.NoError(t, err)
	for _, e := range entries {
		if e.ID == "file-filter" {
			require.Equal(t, firstGen, e.Generation)
		}
	}

	// Editing the file updates the registration under a new generation.
	require.NoError(t, os.WriteFile(filePath, []byte(`
driver: builtin/filter-list
priority: 7
configuration: {reason: second}
`), 0o644))
	require.NoError(t, s.SyncPluginDir(ctx))
	entries, err = s.Entries(ctx)
	require.NoError(t, err)
	for _, e := range entries {
		if e.ID == "file-filter" {
			require.Equal(t, 7, e.Priority)
			require.NotEqual(t, firstGen, e.Generation)
		}
	}

	// Ownership persists across restarts: a fresh store (empty
	// in-memory marks) still knows the file owns this registration...
	for _, e := range entries {
		if e.ID == "file-filter" {
			require.Equal(t, "file", e.Source)
		}
		if e.ID == "api-owned" {
			require.Equal(t, "api", e.Source)
		}
	}
	fresh, err := NewStore(s.cfg, s.pool, s.policy)
	require.NoError(t, err)
	fresh.cfg.PluginsDir = dir
	s = fresh

	// ...so removing the file deregisters only the file-managed plugin.
	require.NoError(t, os.Remove(filePath))
	require.NoError(t, s.SyncPluginDir(ctx))
	entries, err = s.Entries(ctx)
	require.NoError(t, err)
	ids := []string{}
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	require.NotContains(t, ids, "file-filter")
	require.Contains(t, ids, "api-owned")
}
