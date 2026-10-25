package processors

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

func writePluginFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestParsePluginFileDefaultsFromFilename(t *testing.T) {
	dir := t.TempDir()
	path := writePluginFile(t, dir, "adult-filter.plugin.yml", `
driver: builtin/filter-list
configuration:
  block_patterns: ["\\bxxx\\b"]
  reason: "adult"
`)
	spec, err := ParsePluginFile(path)
	require.NoError(t, err)
	require.Equal(t, "adult-filter", spec.ID)
	require.True(t, spec.Enabled)
	w := spec.Write()
	require.Equal(t, v1.DriverBuiltinFilter, w.Driver)
	require.Equal(t, "shadow", w.Mode)
	require.Equal(t, "active", w.State)
	require.Equal(t, defaultPluginPriority, w.Priority)
	require.JSONEq(t, `{"block_patterns":["\\bxxx\\b"],"reason":"adult"}`, string(w.Configuration))
	require.NoError(t, validateWrite(w))
}

func TestParsePluginFileHTTPService(t *testing.T) {
	dir := t.TempDir()
	path := writePluginFile(t, dir, "matcher.plugin.yml", `
id: my-matcher
endpoint: http://matcher:8090
mode: apply
priority: 20
auth_env: DH_WAYPOINT_PROCESSOR_TOKEN_MATCHER
selector:
  content_types: [movie]
configuration: {}
`)
	spec, err := ParsePluginFile(path)
	require.NoError(t, err)
	w := spec.Write()
	require.Equal(t, "my-matcher", spec.ID)
	require.Equal(t, "", w.Driver) // empty means http
	require.Equal(t, "http://matcher:8090", w.Endpoint)
	require.Equal(t, "apply", w.Mode)
	require.Equal(t, 20, w.Priority)
	require.NotNil(t, w.Selector)
	require.Equal(t, []string{"movie"}, w.Selector.ContentTypes)
	require.NoError(t, validateWrite(w))
}

func TestParsePluginFileRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := writePluginFile(t, dir, "typo.plugin.yml", `
driver: builtin/filter-list
priorty: 5
configuration: {}
`)
	_, err := ParsePluginFile(path)
	require.Error(t, err, "misspelled keys must fail loudly, not be ignored")
}

func TestParsePluginFileRejectsInvalidMode(t *testing.T) {
	dir := t.TempDir()
	path := writePluginFile(t, dir, "bad.plugin.yml", `
driver: builtin/filter-list
mode: yolo
configuration: {}
`)
	_, err := ParsePluginFile(path)
	require.Error(t, err)
}

func TestLoadPluginDirMissingIsEmpty(t *testing.T) {
	specs, err := LoadPluginDir(filepath.Join(t.TempDir(), "does-not-exist"))
	require.NoError(t, err)
	require.Empty(t, specs)
}

func TestLoadPluginDirDisabledFileStillParsed(t *testing.T) {
	dir := t.TempDir()
	writePluginFile(t, dir, "off.plugin.yml", `
driver: builtin/filter-list
enabled: false
configuration: {reason: test}
`)
	specs, err := LoadPluginDir(dir)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.False(t, specs[0].Enabled)
}

// The templates in plugins/ are what users switch on in place; keep them
// parseable, registrable and off until a user enables one.
func TestShippedPluginTemplatesAreValid(t *testing.T) {
	dir := filepath.Join("..", "..", "plugins")
	specs, err := LoadPluginDir(dir)
	require.NoError(t, err)
	require.Len(t, specs, 3)
	store := &Store{}
	for _, spec := range specs {
		w := spec.Write()
		require.False(t, spec.Enabled, "%s must ship disabled", spec.Path)
		if normalizedDriver(w.Driver) == v1.DriverHTTP {
			continue // registration probes the live endpoint
		}
		_, _, err := store.inspectBuiltin(context.Background(), spec.ID, w)
		require.NoError(t, err, "%s must pass builtin registration checks", spec.Path)
	}
}

func TestPluginWriteMatchesEntry(t *testing.T) {
	dir := t.TempDir()
	path := writePluginFile(t, dir, "f.plugin.yml", `
driver: builtin/filter-list
mode: shadow
state: active
priority: 100
configuration: {reason: test}
`)
	spec, err := ParsePluginFile(path)
	require.NoError(t, err)
	entry := Entry{
		Source:        "file",
		ID:            "f",
		Driver:        v1.DriverBuiltinFilter,
		Mode:          "shadow",
		State:         "active",
		Priority:      100,
		Configuration: []byte(`{"reason":"test"}`),
	}
	require.True(t, spec.Matches(entry), "identical spec and entry should match")
	entry.Priority = 5
	require.False(t, spec.Matches(entry), "priority drift must be detected")
}
