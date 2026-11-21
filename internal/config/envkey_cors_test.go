package config

import (
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/stretchr/testify/require"
)

// The security page tells operators to name browser origins with this variable.
func TestEnvBinding_HTTPServerCorsAllowedOrigins(t *testing.T) {
	env := map[string]string{"HTTP_SERVER_CORS_ALLOWED_ORIGINS": "https://a.example,https://b.example"}
	resolved := resolveSectionFromEnv(t, "http_server", httpserver.NewDefaultConfig(), env)
	require.Equal(t, []string{"https://a.example", "https://b.example"}, resolved.(httpserver.Config).Cors.AllowedOrigins)

	resolved = resolveSectionFromEnv(t, "http_server", httpserver.NewDefaultConfig(), map[string]string{})
	require.Empty(t, resolved.(httpserver.Config).Cors.AllowedOrigins)
}

func TestEnvBinding_HTTPServerScopedAPIKeysAndCSRF(t *testing.T) {
	env := map[string]string{
		"HTTP_SERVER_SCOPED_API_KEYS":        "prom:metrics:syn-metrics-secret,ui:search:syn-search-secret",
		"HTTP_SERVER_CROSS_ORIGIN_PROTECTION": "false",
	}
	resolved := resolveSectionFromEnv(t, "http_server", httpserver.NewDefaultConfig(), env).(httpserver.Config)
	require.Equal(t, "prom:metrics:syn-metrics-secret,ui:search:syn-search-secret", string(resolved.ScopedAPIKeys))
	require.Equal(t, "[redacted]", resolved.ScopedAPIKeys.String())
	require.False(t, resolved.CrossOriginProtection)

	defaults := resolveSectionFromEnv(t, "http_server", httpserver.NewDefaultConfig(), map[string]string{}).(httpserver.Config)
	require.Empty(t, string(defaults.ScopedAPIKeys))
	require.True(t, defaults.CrossOriginProtection)
}
