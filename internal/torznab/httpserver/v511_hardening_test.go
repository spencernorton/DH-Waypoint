package httpserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/spencernorton/DH-Waypoint/internal/torznab/httpserver"
	torznab_mocks "github.com/spencernorton/DH-Waypoint/internal/torznab/mocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigAppliesNewestOrderOffsetCapTimeoutAndCustomDefaultProfile(t *testing.T) {
	cfg := torznab.NewDefaultConfig()
	cfg.MaxOffset = 500
	cfg.RequestTimeout = 15 * time.Second

	clientMock := torznab_mocks.NewClient(t)
	lazyClient := lazy.New[torznab.Client](func() (torznab.Client, error) { return clientMock, nil })
	engine := gin.New()
	require.NoError(t, httpserver.New(lazyClient, cfg, httpserver.NewMetrics()).Apply(engine))

	clientMock.EXPECT().Search(mock.Anything, mock.Anything).Run(func(ctx context.Context, req torznab.SearchRequest) {
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline, "default config attaches RequestTimeout when caller context has none")
		require.True(t, req.Profile.DisableOrderByRelevance, "DefaultOrder=newest disables broad relevance rank by default")
		require.True(t, req.Offset.Valid)
		require.Equal(t, uint(500), req.Offset.Uint, "offset above MaxOffset is clamped")
	}).Return(torznab.SearchResult{}, nil).Once()

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/torznab/api?t=search&q=ubuntu&offset=999999", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	cfgWithCustomDefault := torznab.NewDefaultConfig()
	cfgWithCustomDefault.Profiles = []torznab.Profile{{
		ID:                      "default",
		Title:                   "CustomDefault",
		DisableOrderByRelevance: false,
		DefaultLimit:            50,
		MaxLimit:                75,
	}}
	cfgWithCustomDefault = cfgWithCustomDefault.MergeDefaults()
	engine2 := gin.New()
	require.NoError(t, httpserver.New(lazyClient, cfgWithCustomDefault, httpserver.NewMetrics()).Apply(engine2))
	rec2 := httptest.NewRecorder()
	engine2.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/torznab/api?t=caps", nil))
	require.Equal(t, http.StatusOK, rec2.Code)
	require.Contains(t, rec2.Body.String(), "CustomDefault")
}
