package httpserver

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
)

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
// import, metrics, REST) behind one optional key
// (HTTP_SERVER_API_KEY). Unset preserves the historic open
// behaviour for trusted networks. Bearer and X-API-Key headers are
// both accepted; comparison is constant-time.
func APIKeyAuth(key namepolicy.SecretToken) gin.HandlerFunc {
	return func(c *gin.Context) {
		// A browser sends a CORS preflight without credentials, and it always
		// names the method it asks about. Any other OPTIONS request is an
		// ordinary request and needs the key.
		preflight := c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Method") != ""
		if string(key) == "" || preflight || apiKeyExemptPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		presented := c.GetHeader("X-API-Key")
		if presented == "" {
			presented = strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(key)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}
