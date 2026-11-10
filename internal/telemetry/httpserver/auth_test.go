package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/cors"
	gincors "github.com/rs/cors/wrapper/gin"
	core "github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
)

// With an API key set, the metrics and profiling pages must need it for every
// method. They used to answer OPTIONS like GET, and OPTIONS skipped the key.
func TestDebugPagesNeedTheAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, passthrough := range []bool{false, true} {
		g := gin.New()
		g.Use(core.APIKeyAuth("secret"))
		g.Use(gincors.New(cors.Options{
			AllowedOrigins:     []string{"https://app.example"},
			OptionsPassthrough: passthrough,
		}))

		registry := prometheus.NewRegistry()
		registry.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "probe", Help: "probe"}))

		for _, option := range []core.Option{
			pprofBuilder{},
			prometheusBuilder{registry: lazy.New(func() (*prometheus.Registry, error) { return registry, nil })},
		} {
			if err := option.Apply(g); err != nil {
				t.Fatal(err)
			}
		}

		do := func(method, path string, header ...string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, path, nil)
			for i := 0; i+1 < len(header); i += 2 {
				req.Header.Set(header[i], header[i+1])
			}

			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)

			return rec
		}

		for _, path := range []string{"/metrics", "/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/goroutine?debug=1", "/debug/pprof/heap"} {
			if rec := do(http.MethodGet, path, "X-API-Key", "secret"); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
				t.Errorf("GET %s with the key: status %d, %d bytes", path, rec.Code, rec.Body.Len())
			}

			for _, method := range []string{http.MethodGet, http.MethodOptions, http.MethodPost, http.MethodHead} {
				if rec := do(method, path); rec.Code != http.StatusUnauthorized {
					t.Errorf("%s %s without the key: status %d, want 401", method, path, rec.Code)
				}
			}

			// A preflight skips the key check, so it must never reach the page.
			rec := do(http.MethodOptions, path, "Origin", "https://app.example", "Access-Control-Request-Method", "GET")
			if rec.Body.Len() != 0 {
				t.Errorf("preflight to %s (passthrough=%v) returned %d bytes: status %d", path, passthrough, rec.Body.Len(), rec.Code)
			}
		}
	}
}
