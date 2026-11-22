package httpserver

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// APIKeyScope defines the least-privilege authorization tier granted to a
// configured HTTP API key.
type APIKeyScope string

const (
	ScopeAdmin   APIKeyScope = "admin"
	ScopeRead    APIKeyScope = "read"
	ScopeSearch  APIKeyScope = "search"
	ScopeMetrics APIKeyScope = "metrics"
	ScopeImport  APIKeyScope = "import"

	// ContextKeyAPIKeyName stores the non-secret logical name of the
	// authenticated API key on the Gin context.
	ContextKeyAPIKeyName = "httpserver.api_key_name"
	// ContextKeyAPIKeyScope stores the authenticated APIKeyScope string on
	// the Gin context.
	ContextKeyAPIKeyScope = "httpserver.api_key_scope"

	maxGraphQLInspectBytes = 1 << 20
)

// ScopedAPIKey represents one named, scoped HTTP API key.
type ScopedAPIKey struct {
	Name  string
	Scope APIKeyScope
	Token namepolicy.SecretToken
}

func validAPIKeyScope(s APIKeyScope) bool {
	switch s {
	case ScopeAdmin, ScopeRead, ScopeSearch, ScopeMetrics, ScopeImport:
		return true
	default:
		return false
	}
}

func validAPIKeyName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// ParseScopedAPIKeys parses comma- or newline-separated `name:scope:secret`
// entries into validated ScopedAPIKey records. Error messages never echo secret
// token material.
func ParseScopedAPIKeys(raw namepolicy.SecretToken) ([]ScopedAPIKey, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	seenNames := make(map[string]struct{}, len(parts))
	keys := make([]ScopedAPIKey, 0, len(parts))
	for i, part := range parts {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		fields := strings.SplitN(entry, ":", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("http_server.scoped_api_keys entry %d must use name:scope:secret format", i+1)
		}
		name := strings.TrimSpace(fields[0])
		scope := APIKeyScope(strings.ToLower(strings.TrimSpace(fields[1])))
		token := strings.TrimSpace(fields[2])
		if !validAPIKeyName(name) {
			return nil, fmt.Errorf("http_server.scoped_api_keys entry %d has invalid key name %q", i+1, name)
		}
		if _, dup := seenNames[name]; dup {
			return nil, fmt.Errorf("http_server.scoped_api_keys has duplicate key name %q", name)
		}
		if !validAPIKeyScope(scope) {
			return nil, fmt.Errorf("http_server.scoped_api_keys entry %q has unknown scope %q (allowed: admin, read, search, metrics, import)", name, scope)
		}
		if token == "" {
			return nil, fmt.Errorf("http_server.scoped_api_keys entry %q has an empty secret token", name)
		}
		seenNames[name] = struct{}{}
		keys = append(keys, ScopedAPIKey{
			Name:  name,
			Scope: scope,
			Token: namepolicy.SecretToken(token),
		})
	}
	return keys, nil
}

// API-key exempt prefixes carry their own authentication and are
// never double-gated: Torznab (apikey), the tracker (URL passkey),
// the processors operator API (admin token), the private file API
// (admin token/passkey), evidence webhooks (per-instance secret),
// the payload verifier API and the internal name-policy check (each
// its own bearer token, refused while that token is unset). The last
// two send their token in the Authorization header this key would
// otherwise claim. /status stays public as the liveness probe.
var apiKeyExempt = []string{
	"/status", "/torznab", "/announce", "/scrape", "/api/processors", "/api/private", "/evidence",
	"/api/publication", "/internal/name-policy",
}

func apiKeyExemptPath(path string) bool {
	for _, prefix := range apiKeyExempt {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// APIKeyAuth gates the otherwise unauthenticated surfaces (GraphQL,
// import, metrics, REST) behind one optional admin key
// (HTTP_SERVER_API_KEY). Unset preserves the historic open
// behaviour for trusted networks. Bearer and X-API-Key headers are
// both accepted; comparison is constant-time.
func APIKeyAuth(key namepolicy.SecretToken) gin.HandlerFunc {
	return ScopedAPIKeyAuth(key, nil)
}

// ScopedAPIKeyAuth enforces authentication and least-privilege scope checks
// across the legacy admin key and any configured named scoped keys. When both
// are empty, requests pass through unchanged.
func ScopedAPIKeyAuth(legacyAdminKey namepolicy.SecretToken, scoped []ScopedAPIKey) gin.HandlerFunc {
	keys := make([]ScopedAPIKey, 0, len(scoped)+1)
	if string(legacyAdminKey) != "" {
		keys = append(keys, ScopedAPIKey{
			Name:  "default",
			Scope: ScopeAdmin,
			Token: legacyAdminKey,
		})
	}
	keys = append(keys, scoped...)

	return func(c *gin.Context) {
		// A browser sends a CORS preflight without credentials, and it always
		// names the method it asks about. Any other OPTIONS request is an
		// ordinary request and needs the key.
		preflight := c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Method") != ""
		if len(keys) == 0 || preflight || apiKeyExemptPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		presented := c.GetHeader("X-API-Key")
		if presented == "" {
			presented = strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		}
		// Compare against all configured keys without early exit to avoid
		// leaking key count or slot order via response timing.
		matchedIdx := -1
		for i, k := range keys {
			if subtle.ConstantTimeCompare([]byte(presented), []byte(k.Token)) == 1 && matchedIdx == -1 {
				matchedIdx = i
			}
		}
		if matchedIdx == -1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		matched := keys[matchedIdx]
		c.Set(ContextKeyAPIKeyName, matched.Name)
		c.Set(ContextKeyAPIKeyScope, string(matched.Scope))

		if status, code := authorizeScope(matched.Scope, c); status != 0 {
			c.AbortWithStatusJSON(status, gin.H{"error": code, "scope": string(matched.Scope)})
			return
		}
		c.Next()
	}
}

func authorizeScope(scope APIKeyScope, c *gin.Context) (int, string) {
	if scope == ScopeAdmin {
		return 0, ""
	}
	method := c.Request.Method
	path := c.Request.URL.Path
	isReadMethod := method == http.MethodGet || method == http.MethodHead

	switch scope {
	case ScopeMetrics:
		if isReadMethod && path == "/metrics" {
			return 0, ""
		}
		return http.StatusForbidden, "forbidden"
	case ScopeImport:
		if method == http.MethodPost && (path == "/import" || strings.HasPrefix(path, "/import/")) {
			return 0, ""
		}
		return http.StatusForbidden, "forbidden"
	case ScopeSearch:
		return authorizeSearchOrRead(scope, isReadMethod, method, path, c, false)
	case ScopeRead:
		return authorizeSearchOrRead(scope, isReadMethod, method, path, c, true)
	default:
		return http.StatusForbidden, "forbidden"
	}
}

func authorizeSearchOrRead(_ APIKeyScope, isReadMethod bool, method, path string, c *gin.Context, allowMetrics bool) (int, string) {
	if allowMetrics && isReadMethod && path == "/metrics" {
		return 0, ""
	}
	if isReadMethod && (strings.HasPrefix(path, "/api/v1/") || strings.HasPrefix(path, "/api/v2/")) {
		return 0, ""
	}
	if method == http.MethodPost && path == "/graphql" {
		return authorizeReadOnlyGraphQL(c)
	}
	return http.StatusForbidden, "forbidden"
}

type graphQLAuthPayload struct {
	Query         string `json:"query"`
	OperationName string `json:"operationName"`
}

// authorizeReadOnlyGraphQL parses the GraphQL document with gqlparser and
// ensures the request executes only GraphQL Query operations (rejecting
// mutations, subscriptions, empty/APQ-only documents, and malformed payloads).
func authorizeReadOnlyGraphQL(c *gin.Context) (int, string) {
	if c.Request.Body == nil {
		return http.StatusForbidden, "forbidden"
	}
	if c.Request.ContentLength > maxGraphQLInspectBytes {
		return http.StatusRequestEntityTooLarge, "request_body_too_large"
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxGraphQLInspectBytes+1))
	if err != nil {
		return http.StatusBadRequest, "request_body_unreadable"
	}
	if len(raw) > maxGraphQLInspectBytes {
		return http.StatusRequestEntityTooLarge, "request_body_too_large"
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	var payload graphQLAuthPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return http.StatusForbidden, "forbidden"
	}
	if strings.TrimSpace(payload.Query) == "" {
		// Reject blind persisted-query hashes under read-only scopes because
		// a cached operation cannot be verified as a query before execution.
		return http.StatusForbidden, "forbidden"
	}
	doc, parseErr := parser.ParseQuery(&ast.Source{Input: payload.Query})
	if parseErr != nil || doc == nil || len(doc.Operations) == 0 {
		return http.StatusForbidden, "forbidden"
	}
	fragments := make(map[string]*ast.FragmentDefinition, len(doc.Fragments))
	for _, frag := range doc.Fragments {
		fragments[frag.Name] = frag
	}
	if opName := strings.TrimSpace(payload.OperationName); opName != "" {
		var target *ast.OperationDefinition
		for _, op := range doc.Operations {
			if op.Name == opName {
				target = op
				break
			}
		}
		if target == nil || target.Operation != ast.Query || selectionSetUsesAdminOnlyField(target.SelectionSet, fragments, map[string]bool{}) {
			return http.StatusForbidden, "forbidden"
		}
		return 0, ""
	}
	for _, op := range doc.Operations {
		if op.Operation != ast.Query || selectionSetUsesAdminOnlyField(op.SelectionSet, fragments, map[string]bool{}) {
			return http.StatusForbidden, "forbidden"
		}
	}
	return 0, ""
}

// selectionSetUsesAdminOnlyField rejects top-level GraphQL fields that expose
// catalogue-policy-hidden evidence rows under non-admin scopes (including via
// aliases, inline fragments, or named fragment spreads).
func selectionSetUsesAdminOnlyField(set ast.SelectionSet, fragments map[string]*ast.FragmentDefinition, visited map[string]bool) bool {
	for _, sel := range set {
		switch node := sel.(type) {
		case *ast.Field:
			if node.Name == "evidence" {
				return true
			}
		case *ast.InlineFragment:
			if selectionSetUsesAdminOnlyField(node.SelectionSet, fragments, visited) {
				return true
			}
		case *ast.FragmentSpread:
			if visited[node.Name] {
				continue
			}
			visited[node.Name] = true
			if frag, ok := fragments[node.Name]; ok && selectionSetUsesAdminOnlyField(frag.SelectionSet, fragments, visited) {
				return true
			}
		}
	}
	return false
}
