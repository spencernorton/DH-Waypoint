package processors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
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

func TestPluginDirReRegistersOnServiceIdentityMismatch(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := testContext
	dir := t.TempDir()
	s.cfg.PluginsDir = dir

	var modelVer atomic.Value
	modelVer.Store("armiger-3.2.1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := v1.Identity{
			ID:             "armiger-ultra",
			Version:        "1.0.0",
			ModelVersion:   modelVer.Load().(string),
			PolicyVersion:  "min-confidence-0.60",
			ArtifactDigest: "none",
		}
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(v1.Manifest{
				Contract:            v1.Contract,
				Processor:           id,
				Capabilities:        []v1.Stage{v1.Classify},
				ConfigurationSchema: json.RawMessage(`{"type":"object"}`),
				Qualification:       "unqualified",
			})
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(v1.ServiceStatus{
				Contract:   v1.Contract,
				Processor:  id,
				State:      "ready",
				InstanceID: "inst-1",
				StartedAt:  "2026-01-01T00:00:00Z",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	filePath := filepath.Join(dir, "armiger-ultra.plugin.yml")
	require.NoError(t, os.WriteFile(filePath, []byte("id: armiger-ultra\nendpoint: "+srv.URL+"\nmode: apply\nconfiguration: {}\n"), 0o644))

	require.NoError(t, s.SyncPluginDir(ctx))
	entries, err := s.Entries(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "armiger-3.2.1", entries[0].Manifest.Processor.ModelVersion)
	gen1 := entries[0].Generation

	// Service restarts with a new model version; Refresh marks service_identity_mismatch,
	// and SyncPluginDir re-registers the file-owned plugin with the new manifest.
	modelVer.Store("armiger-3.8.1")
	require.NoError(t, s.Refresh(ctx))
	require.NoError(t, s.SyncPluginDir(ctx))

	entries, err = s.Entries(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "armiger-3.8.1", entries[0].Manifest.Processor.ModelVersion)
	require.Equal(t, "", entries[0].LastError)
	require.Greater(t, entries[0].Generation, gen1)
}
