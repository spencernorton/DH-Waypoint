package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/stretchr/testify/require"
)

func apiKeyEngine(key string) *gin.Engine {
	return scopedAPIKeyEngine(key, nil)
}

func scopedAPIKeyEngine(legacyKey string, scoped []ScopedAPIKey) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(ScopedAPIKeyAuth(namepolicy.SecretToken(legacyKey), scoped))
	for _, p := range []string{
		"/graphql", "/metrics", "/import", "/import/torrent",
		"/api/v1/search", "/api/v2/search", "/api/quarantine", "/debug/pprof/",
		"/status", "/torznab/api", "/announce/k", "/api/processors/v1/status",
		"/evidence/arr/x", "/api/publication/v1/lease", "/internal/name-policy/check",
	} {
		handler := func(c *gin.Context) {
			if name, ok := c.Get(ContextKeyAPIKeyName); ok {
				c.Header("X-Test-Key-Name", name.(string))
			}
			if scope, ok := c.Get(ContextKeyAPIKeyScope); ok {
				c.Header("X-Test-Key-Scope", scope.(string))
			}
			if c.Request.Body != nil {
				body, _ := io.ReadAll(c.Request.Body)
				c.Data(http.StatusOK, "application/json", body)
				return
			}
			c.Status(http.StatusOK)
		}
		e.GET(p, handler)
		e.HEAD(p, handler)
		e.POST(p, handler)
	}
	return e
}

func call(t *testing.T, e *gin.Engine, method, path, header, value string) int {
	t.Helper()
	return callWithBody(t, e, method, path, header, value, "").Code
}

func callWithBody(t *testing.T, e *gin.Engine, method, path, header, value, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
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

func TestParseScopedAPIKeysValidationAndRedaction(t *testing.T) {
	keys, err := ParseScopedAPIKeys(namepolicy.SecretToken("prom:metrics:tok-1,\nsearch-ui:search:tok:with:colons,reader:read:tok-3"))
	require.NoError(t, err)
	require.Len(t, keys, 3)
	require.Equal(t, "prom", keys[0].Name)
	require.Equal(t, ScopeMetrics, keys[0].Scope)
	require.Equal(t, namepolicy.SecretToken("tok-1"), keys[0].Token)
	require.Equal(t, namepolicy.SecretToken("tok:with:colons"), keys[1].Token)
	require.Equal(t, "[redacted]", keys[1].Token.String())

	// Malformed entries fail closed and never echo secrets.
	secret := "do-not-leak-this-token"
	for _, bad := range []string{
		"missingcolon",
		"bad name!:read:" + secret,
		"dup:read:a,dup:metrics:b",
		"ops:superuser:" + secret,
		"empty:read:",
	} {
		_, err := ParseScopedAPIKeys(namepolicy.SecretToken(bad))
		require.Error(t, err, bad)
		require.NotContains(t, err.Error(), secret)
	}
}

func TestScopedAPIKeyAuthorizationMatrix(t *testing.T) {
	keys, err := ParseScopedAPIKeys(namepolicy.SecretToken(strings.Join([]string{
		"prom:metrics:tok-metrics",
		"ui:search:tok-search",
		"reader:read:tok-read",
		"bulk:import:tok-import",
		"ops:admin:tok-admin",
	}, ",")))
	require.NoError(t, err)
	e := scopedAPIKeyEngine("", keys)

	queryBody := `{"query":"query Find { torrentContent { search { totalCount } } }"}`
	mutationBody := `{"query":"mutation Wipe { queue { purgeJobs(queue: \"default\") } }"}`
	multiOpBody := `{"query":"query Q { a } mutation M { b }"}`
	multiOpSelectQ := `{"operationName":"Q","query":"query Q { a } mutation M { b }"}`
	multiOpSelectM := `{"operationName":"M","query":"query Q { a } mutation M { b }"}`
	apqOnlyBody := `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"deadbeef"}}}`

	// Unauthenticated requests are rejected once any scoped key is configured.
	require.Equal(t, 401, call(t, e, "GET", "/metrics", "", ""))
	require.Equal(t, 401, call(t, e, "GET", "/api/v1/search", "", ""))

	// Metrics key: only GET/HEAD /metrics.
	rec := callWithBody(t, e, "GET", "/metrics", "Authorization", "Bearer tok-metrics", "")
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "prom", rec.Header().Get("X-Test-Key-Name"))
	require.Equal(t, "metrics", rec.Header().Get("X-Test-Key-Scope"))
	require.Equal(t, 200, call(t, e, "HEAD", "/metrics", "X-API-Key", "tok-metrics"))
	require.Equal(t, 403, call(t, e, "GET", "/api/v1/search", "X-API-Key", "tok-metrics"))
	require.Equal(t, 403, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-metrics", queryBody).Code)
	require.Equal(t, 403, call(t, e, "POST", "/import", "X-API-Key", "tok-metrics"))
	require.Equal(t, 403, call(t, e, "GET", "/api/quarantine", "X-API-Key", "tok-metrics"))
	require.Equal(t, 403, call(t, e, "GET", "/debug/pprof/", "X-API-Key", "tok-metrics"))

	// Search key: REST reads + read-only GraphQL queries (rewinds body), no /metrics, no mutations, no /import.
	require.Equal(t, 200, call(t, e, "GET", "/api/v1/search", "X-API-Key", "tok-search"))
	require.Equal(t, 200, call(t, e, "GET", "/api/v2/search", "X-API-Key", "tok-search"))
	rec = callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-search", queryBody)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, queryBody, rec.Body.String(), "middleware must rewind body for GraphQL handler")
	require.Equal(t, "ui", rec.Header().Get("X-Test-Key-Name"))
	require.Equal(t, 200, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-search", multiOpSelectQ).Code)
	require.Equal(t, 403, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-search", mutationBody).Code)
	require.Equal(t, 403, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-search", multiOpBody).Code)
	require.Equal(t, 403, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-search", multiOpSelectM).Code)
	require.Equal(t, 403, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-search", apqOnlyBody).Code)
	require.Equal(t, 403, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-search", `{"query":"not valid graphql {"}`).Code)
	require.Equal(t, 403, call(t, e, "GET", "/metrics", "X-API-Key", "tok-search"))
	require.Equal(t, 403, call(t, e, "POST", "/import", "X-API-Key", "tok-search"))
	require.Equal(t, 403, call(t, e, "GET", "/api/quarantine", "X-API-Key", "tok-search"))

	// Read key: metrics + search, still no mutations/import/quarantine/pprof.
	require.Equal(t, 200, call(t, e, "GET", "/metrics", "X-API-Key", "tok-read"))
	require.Equal(t, 200, call(t, e, "GET", "/api/v1/search", "X-API-Key", "tok-read"))
	require.Equal(t, 200, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-read", queryBody).Code)
	require.Equal(t, 403, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-read", mutationBody).Code)
	require.Equal(t, 403, call(t, e, "POST", "/import", "X-API-Key", "tok-read"))
	require.Equal(t, 403, call(t, e, "GET", "/api/quarantine", "X-API-Key", "tok-read"))
	require.Equal(t, 403, call(t, e, "GET", "/debug/pprof/", "X-API-Key", "tok-read"))

	// Import key: only POST /import and /import/*.
	require.Equal(t, 200, call(t, e, "POST", "/import", "X-API-Key", "tok-import"))
	require.Equal(t, 200, call(t, e, "POST", "/import/torrent", "X-API-Key", "tok-import"))
	require.Equal(t, 403, call(t, e, "GET", "/import", "X-API-Key", "tok-import"))
	require.Equal(t, 403, call(t, e, "GET", "/api/v1/search", "X-API-Key", "tok-import"))
	require.Equal(t, 403, call(t, e, "GET", "/metrics", "X-API-Key", "tok-import"))

	// Admin key: full access including GraphQL mutations, import, quarantine, pprof.
	require.Equal(t, 200, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "tok-admin", mutationBody).Code)
	require.Equal(t, 200, call(t, e, "POST", "/import", "X-API-Key", "tok-admin"))
	require.Equal(t, 200, call(t, e, "GET", "/api/quarantine", "X-API-Key", "tok-admin"))
	require.Equal(t, 200, call(t, e, "GET", "/debug/pprof/", "X-API-Key", "tok-admin"))
}

func TestReadOnlyGraphQLRejectsEvidenceField(t *testing.T) {
	e := scopedAPIKeyEngine("", []ScopedAPIKey{
		{Name: "reader", Scope: ScopeRead, Token: "read-tok"},
		{Name: "searcher", Scope: ScopeSearch, Token: "search-tok"},
		{Name: "ops", Scope: ScopeAdmin, Token: "admin-tok"},
	})
	blockedQueries := []string{
		`{"query":"query { evidence { totalCount } }"}`,
		`{"query":"query { e: evidence { totalCount } }"}`,
		`{"query":"query { ... on Query { evidence { totalCount } } }"}`,
		`{"query":"query { ...Ev } fragment Ev on Query { evidence { totalCount } }"}`,
		`{"query":"query A { search { totalCount } } query B { evidence { totalCount } }","operationName":"B"}`,
	}
	for _, body := range blockedQueries {
		require.Equal(t, http.StatusForbidden, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "read-tok", body).Code, body)
		require.Equal(t, http.StatusForbidden, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "search-tok", body).Code, body)
		require.Equal(t, http.StatusOK, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "admin-tok", body).Code, body)
	}
	allowed := `{"query":"query A { search { totalCount } } query B { evidence { totalCount } }","operationName":"A"}`
	require.Equal(t, http.StatusOK, callWithBody(t, e, "POST", "/graphql", "X-API-Key", "read-tok", allowed).Code)
}
