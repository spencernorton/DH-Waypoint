package httpserver

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// NewCrossOriginProtectionMiddleware builds Gin middleware backed by Go's
// standard net/http.CrossOriginProtection. Safe methods (GET, HEAD, OPTIONS)
// and non-browser clients without Sec-Fetch-Site/Origin headers pass through;
// browser cross-site state-changing requests (POST, PUT, PATCH, DELETE) are
// blocked with 403 unless the origin is explicitly trusted via
// Cors.AllowedOrigins or protection is disabled.
func NewCrossOriginProtectionMiddleware(cfg Config) (gin.HandlerFunc, error) {
	if !cfg.CrossOriginProtection {
		return func(c *gin.Context) { c.Next() }, nil
	}

	cop := http.NewCrossOriginProtection()
	var wildcardPatterns []string
	for _, rawOrigin := range cfg.Cors.AllowedOrigins {
		origin := strings.TrimSpace(rawOrigin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			// Operator explicitly allowed every browser origin via CORS.
			return func(c *gin.Context) { c.Next() }, nil
		}
		if strings.Contains(origin, "*") {
			wildcardPatterns = append(wildcardPatterns, origin)
			continue
		}
		if err := cop.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("http_server.cors.allowed_origins entry %q: %w", origin, err)
		}
	}

	return func(c *gin.Context) {
		if err := cop.Check(c.Request); err == nil {
			c.Next()
			return
		}
		if origin := c.GetHeader("Origin"); origin != "" && matchesWildcardOrigin(wildcardPatterns, origin) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cross_origin_forbidden"})
	}, nil
}

func matchesWildcardOrigin(patterns []string, origin string) bool {
	if len(patterns) == 0 {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	for _, p := range patterns {
		idx := strings.IndexByte(p, '*')
		if idx < 0 {
			if p == origin {
				return true
			}
			continue
		}
		prefix := p[:idx]
		suffix := p[idx+1:]
		if len(origin) >= len(prefix)+len(suffix) &&
			strings.HasPrefix(origin, prefix) &&
			strings.HasSuffix(origin, suffix) {
			return true
		}
	}
	return false
}
