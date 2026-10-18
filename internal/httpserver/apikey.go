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
// (admin token/passkey) and evidence webhooks (per-instance
// secret). /status stays public as the liveness probe.
var apiKeyExempt = []string{"/status", "/torznab", "/announce", "/scrape", "/api/processors", "/api/private", "/evidence"}

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
		if string(key) == "" || c.Request.Method == http.MethodOptions || apiKeyExemptPath(c.Request.URL.Path) {
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
