package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/stretchr/testify/require"
)

func apiKeyEngine(key string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(APIKeyAuth(namepolicy.SecretToken(key)))
	for _, p := range []string{"/graphql", "/metrics", "/import", "/import/torrent", "/api/v1/search", "/api/v2/search", "/status", "/torznab/api", "/announce/k", "/api/processors/v1/status", "/evidence/arr/x", "/api/publication/v1/lease", "/internal/name-policy/check"} {
		e.GET(p, func(c *gin.Context) { c.Status(http.StatusOK) })
		e.POST(p, func(c *gin.Context) { c.Status(http.StatusOK) })
	}
	return e
}

func call(t *testing.T, e *gin.Engine, method, path, header, value string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

func TestAPIKeyUnsetIsOpen(t *testing.T) {
	e := apiKeyEngine("")
	require.Equal(t, 200, call(t, e, "POST", "/graphql", "", ""))
}

func TestAPIKeyEnforced(t *testing.T) {
	e := apiKeyEngine("s3cret-value")
	require.Equal(t, 401, call(t, e, "POST", "/graphql", "", ""))
	require.Equal(t, 401, call(t, e, "GET", "/metrics", "Authorization", "Bearer wrong"))
	require.Equal(t, 200, call(t, e, "POST", "/graphql", "Authorization", "Bearer s3cret-value"))
	require.Equal(t, 200, call(t, e, "GET", "/api/v1/search", "X-API-Key", "s3cret-value"))
	for _, p := range []string{"/import/torrent", "/api/v2/search"} {
		require.Equal(t, 401, call(t, e, "POST", p, "", ""))
		require.Equal(t, 200, call(t, e, "POST", p, "Authorization", "Bearer s3cret-value"))
	}
	// Surfaces with their own auth are exempt.
	for _, p := range []string{"/status", "/torznab/api", "/announce/k", "/api/processors/v1/status"} {
		require.Equal(t, 200, call(t, e, "GET", p, "", ""), p)
	}
	require.Equal(t, 200, call(t, e, "POST", "/evidence/arr/x", "", ""))
	// These two authenticate with their own bearer token, which is not this key.
	for _, p := range []string{"/api/publication/v1/lease", "/internal/name-policy/check"} {
		require.Equal(t, 200, call(t, e, "POST", p, "Authorization", "Bearer their-own-token"), p)
	}
	// A CORS preflight is never blocked; it reaches no handler here.
	require.Equal(t, 404, call(t, e, "OPTIONS", "/graphql", "Access-Control-Request-Method", "POST"))
	// Any other OPTIONS request is an ordinary request.
	require.Equal(t, 401, call(t, e, "OPTIONS", "/graphql", "", ""))
	require.Equal(t, 401, call(t, e, "OPTIONS", "/metrics", "Origin", "https://example.org"))
}
