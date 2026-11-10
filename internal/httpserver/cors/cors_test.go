package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func allowOriginHeader(t *testing.T, config httpserver.Config, method string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, New(Params{Config: config, Logger: zap.NewNop().Sugar()}).Option.Apply(engine))
	engine.POST("/graphql", func(c *gin.Context) { c.Status(http.StatusOK) })
	req := httptest.NewRequest(method, "/graphql", nil)
	req.Header.Set("Origin", "https://other.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Header().Get("Access-Control-Allow-Origin")
}

func TestNoOriginIsAllowedByDefault(t *testing.T) {
	config := httpserver.NewDefaultConfig()
	require.False(t, config.Cors.Debug)
	require.Empty(t, allowOriginHeader(t, config, http.MethodPost))
	require.Empty(t, allowOriginHeader(t, config, http.MethodOptions))

	config.Cors.AllowedOrigins = []string{"https://other.example"}
	require.Equal(t, "https://other.example", allowOriginHeader(t, config, http.MethodPost))
	require.Equal(t, "https://other.example", allowOriginHeader(t, config, http.MethodOptions))
}
