package restapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

type rawSearchProbe struct{ fakeSearch }

func (rawSearchProbe) Torrents(context.Context, ...query.Option) (search.TorrentsResult, error) {
	return search.TorrentsResult{Items: []model.Torrent{{InfoHash: protocol.InfoHash{1}, Name: "Synthetic.txt"}}}, nil
}

func TestV2RejectsMalformedIdentityAndPagingInputs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	a := New(lazy.New(func() (search.ServingSearch, error) { return rawSearchProbe{}, nil }), nil)
	require.NoError(t, a.Apply(e))
	for _, path := range []string{"/api/v2/search?limit=0", "/api/v2/search?limit=101", "/api/v2/search?offset=1", "/api/v2/search?after=abcd", "/api/v2/search?hash=abcd", "/api/v2/search?q=ab", "/api/v2/search?q=" + strings.Repeat("a", 4097), "/api/v2/torrents/abcd", "/api/v2/torrents/" + protocol.InfoHash{1}.String() + "/files?after_index=-1"} {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		require.Equal(t, 400, rec.Code, path+": "+rec.Body.String())
	}
}
