package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func csrfEngine(t *testing.T, cfg Config) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mw, err := NewCrossOriginProtectionMiddleware(cfg)
	require.NoError(t, err)
	e := gin.New()
	e.Use(mw)
	e.GET("/api/v1/search", func(c *gin.Context) { c.Status(http.StatusOK) })
	e.OPTIONS("/graphql", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	e.POST("/graphql", func(c *gin.Context) { c.Status(http.StatusOK) })
	e.POST("/import", func(c *gin.Context) { c.Status(http.StatusOK) })
	return e
}

func TestCrossOriginProtectionDefaultBlocksCrossSitePosts(t *testing.T) {
	cfg := NewDefaultConfig()
	require.True(t, cfg.CrossOriginProtection)
	e := csrfEngine(t, cfg)

	// Non-browser client without Sec-Fetch-Site or Origin passes.
	req := httptest.NewRequest(http.MethodPost, "http://waypoint.local/graphql", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Safe methods pass even when cross-site.
	req = httptest.NewRequest(http.MethodGet, "http://waypoint.local/api/v1/search", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodOptions, "http://waypoint.local/graphql", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// Cross-site POST with Sec-Fetch-Site: cross-site is blocked.
	req = httptest.NewRequest(http.MethodPost, "http://waypoint.local/graphql", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Legacy browser POST without Sec-Fetch-Site but mismatched Origin is blocked.
	req = httptest.NewRequest(http.MethodPost, "http://waypoint.local/import", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Same-origin POST is allowed.
	req = httptest.NewRequest(http.MethodPost, "http://waypoint.local/graphql", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://waypoint.local")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestCrossOriginProtectionHonorsTrustedOriginsAndWildcards(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Cors.AllowedOrigins = []string{"https://ui.example", "https://*.trusted.example"}
	e := csrfEngine(t, cfg)

	// Explicit trusted origin passes.
	req := httptest.NewRequest(http.MethodPost, "http://waypoint.local/graphql", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://ui.example")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Matching wildcard subdomain passes.
	req = httptest.NewRequest(http.MethodPost, "http://waypoint.local/graphql", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://app.trusted.example")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Crafted origin with userinfo or path is rejected even if suffix matches.
	for _, bad := range []string{
		"https://evil.example",
		"https://user@app.trusted.example",
		"https://app.trusted.example/extra",
	} {
		req = httptest.NewRequest(http.MethodPost, "http://waypoint.local/graphql", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", bad)
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, bad)
	}
}

func TestCrossOriginProtectionDisabledOrStarWildcard(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{
			name: "disabled",
			cfg: func() Config {
				c := NewDefaultConfig()
				c.CrossOriginProtection = false
				return c
			}(),
		},
		{
			name: "cors_star",
			cfg: func() Config {
				c := NewDefaultConfig()
				c.Cors.AllowedOrigins = []string{"*"}
				return c
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := csrfEngine(t, tc.cfg)
			req := httptest.NewRequest(http.MethodPost, "http://waypoint.local/graphql", nil)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			req.Header.Set("Origin", "https://other.example")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestCrossOriginProtectionRejectsInvalidOriginConfig(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Cors.AllowedOrigins = []string{"not-a-valid-origin"}
	_, err := NewCrossOriginProtectionMiddleware(cfg)
	require.Error(t, err)
}
