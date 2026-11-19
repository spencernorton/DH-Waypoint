package restapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

// fakeSearch implements ServingSearch by embedding the interface and
// overriding only the two methods the REST layer calls.
type fakeSearch struct {
	search.ServingSearch
	content search.TorrentContentResult
	files   search.TorrentFilesResult
}

func (f fakeSearch) TorrentContent(context.Context, ...query.Option) (search.TorrentContentResult, error) {
	return f.content, nil
}
func (f fakeSearch) TorrentFiles(context.Context, ...query.Option) (search.TorrentFilesResult, error) {
	return f.files, nil
}

func TestSearchCountProvenance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name     string
		count    uint
		estimate bool
		hasNext  bool
	}{
		{name: "exact", count: 42},
		{name: "estimated", count: 1000, estimate: true, hasNext: true},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := search.TorrentContentResult{}
			result.TotalCount = tc.count
			result.TotalCountIsEstimate = tc.estimate
			result.HasNextPage = tc.hasNext
			api := New(lazy.New(func() (search.ServingSearch, error) {
				return fakeSearch{content: result}, nil
			}), nil)
			engine := gin.New()
			require.NoError(t, api.Apply(engine))
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))
			require.Equal(t, http.StatusOK, rec.Code)
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body, "total_count_is_estimate", "even false must be explicit on the wire")
			var response struct {
				TotalCount           uint   `json:"total_count"`
				TotalCountIsEstimate bool   `json:"total_count_is_estimate"`
				HasNextPage          bool   `json:"has_next_page"`
				Items                []Item `json:"items"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Equal(t, tc.count, response.TotalCount)
			require.Equal(t, tc.estimate, response.TotalCountIsEstimate)
			require.Equal(t, tc.hasNext, response.HasNextPage)
			require.NotNil(t, response.Items)

			stats := httptest.NewRecorder()
			engine.ServeHTTP(stats, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
			require.Equal(t, http.StatusOK, stats.Code)
			var totals map[string]any
			require.NoError(t, json.Unmarshal(stats.Body.Bytes(), &totals))
			require.EqualValues(t, tc.count, totals["torrent_contents"])
			require.Contains(t, totals, "torrent_contents_is_estimate")
			require.Equal(t, tc.estimate, totals["torrent_contents_is_estimate"])
		})
	}
}

func TestToItemMapping(t *testing.T) {
	hash := protocol.InfoHash{0xab}
	seeders := uint(12)
	tc := model.TorrentContent{
		InfoHash:    hash,
		ContentType: model.NewNullContentType(model.ContentTypeMovie),
		Size:        1234,
		Seeders:     model.NewNullUint(seeders),
		PublishedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	tc.Torrent.Name = "Some Film 2026"
	tc.Content.Title = "Some Film"
	it := toItem(tc)
	require.Equal(t, hash.String(), it.InfoHash)
	require.Equal(t, "movie", it.ContentType)
	require.Equal(t, "Some Film", it.Title)
	require.NotNil(t, it.Seeders)
	require.Equal(t, seeders, *it.Seeders)
	require.Contains(t, it.Magnet, "magnet:?xt=urn:btih:"+hash.String())
}

// The checked-in documentation copy must never drift from the served
// embedded specification.
func TestOpenAPIDocsCopyMatchesEmbedded(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "openapi.yaml"))
	require.NoError(t, err)
	require.Equal(t, openAPISpec, doc)
	v2, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "openapi-v2.yaml"))
	require.NoError(t, err)
	require.Equal(t, openAPIV2Spec, v2)
}

func TestRESTSearchFilterValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	api := New(lazy.New(func() (search.ServingSearch, error) {
		return fakeSearch{}, nil
	}), nil)
	engine := gin.New()
	require.NoError(t, api.Apply(engine))

	for _, bad := range []string{
		"/api/v1/search?resolution=999p",
		"/api/v1/search?video_source=VHS",
		"/api/v1/search?video_codec=RealVideo",
		"/api/v1/search?hdr=NOPE",
		"/api/v1/search?min_seeders=-1",
		"/api/v1/search?episode=3",
		"/api/v1/search?season=-1",
		"/api/v1/search?season=1&episode=-5",
		"/api/v1/search?imdb=12345",
		"/api/v1/search?tmdb=abc",
		"/api/v1/search?order_by=bogus",
		"/api/v1/search?order_dir=sideways",
		"/api/v1/search?order_by=relevance",
	} {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, bad, nil))
		require.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s", bad)
	}

	for _, good := range []string{
		"/api/v1/search?resolution=2160p&video_source=WEBDL&video_codec=x265&hdr=HDR10&min_seeders=5",
		"/api/v1/search?type=tv_show&tmdb=12345&season=1&episode=2&order_by=seeders&order_dir=desc",
		"/api/v1/search?q=matrix&order_by=relevance",
		"/api/v1/search?imdb=tt0133093",
	} {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, good, nil))
		require.Equal(t, http.StatusOK, rec.Code, "expected 200 for %s: %s", good, rec.Body.String())
	}
}
