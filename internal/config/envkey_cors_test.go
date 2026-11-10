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
