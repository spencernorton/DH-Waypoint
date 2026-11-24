package restapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

type spyServingSearch struct {
	search.ServingSearch
	v1Calls atomic.Int32
	v2Calls atomic.Int32
	v1Items []search.TorrentContentResultItem
	v2Items []model.Torrent
}

func (s *spyServingSearch) TorrentContent(_ context.Context, _ ...query.Option) (search.TorrentContentResult, error) {
	s.v1Calls.Add(1)
	return search.TorrentContentResult{Items: s.v1Items, TotalCount: uint(len(s.v1Items))}, nil
}

func (s *spyServingSearch) Torrents(_ context.Context, _ ...query.Option) (search.TorrentsResult, error) {
	s.v2Calls.Add(1)
	return search.TorrentsResult{Items: s.v2Items}, nil
}

func TestRestAPIDirectServeAndFailClosedPGFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var expired atomic.Bool
	tmsHash := strings.Repeat("c1", 20)
	pgHash := strings.Repeat("d1", 20)
	nowEpoch := uint32(time.Now().Unix())
	totalFacets := uint64(1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if expired.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"governance_lease_expired","code":"lease_expired"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/search":
			_ = json.NewEncoder(w).Encode(dhtms.SearchResponse{
				Docs: []dhtms.DocHit{{
					DocID: 1, InfoHashV1: tmsHash, Name: "TMS.Direct.Movie.2026.1080p",
					SizeBytes: 5_000_000_000, ContentType: 1, Resolution: 6,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch,
				}},
				TotalFacets: &totalFacets,
			})
		case "/v1/substring":
			_ = json.NewEncoder(w).Encode(dhtms.RSSResponse{
				Docs: []dhtms.DocHit{{
					DocID: 1, InfoHashV1: tmsHash, Name: "TMS.Direct.Movie.2026.1080p",
					SizeBytes: 5_000_000_000, ContentType: 1, Resolution: 6,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch, FilesCount: 1,
				}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cli, err := dhtms.NewClient(dhtms.Config{
		Mode:     dhtms.ReadModeServe,
		Endpoint: srv.URL,
		Timeout:  2 * time.Second,
	})
	require.NoError(t, err)
	defer cli.Close()

	pgIH, err := protocol.ParseInfoHash(pgHash)
	require.NoError(t, err)
	spy := &spyServingSearch{
		v1Items: []search.TorrentContentResultItem{{
			TorrentContent: model.TorrentContent{
				InfoHash:    pgIH,
				PublishedAt: time.Now().UTC(),
				Torrent:     model.Torrent{InfoHash: pgIH, Name: "PG.Fallback.V1"},
			},
		}},
		v2Items: []model.Torrent{{InfoHash: pgIH, Name: "PG.Fallback.V2"}},
	}

	api := New(lazy.New(func() (search.ServingSearch, error) { return spy, nil }), nil).
		WithDHTMS(cli, nil, dhtms.NewMetrics(), nil)
	engine := gin.New()
	require.NoError(t, api.Apply(engine))

	doGet := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		engine.ServeHTTP(rec, req)
		return rec
	}

	// 1. /api/v1/search hits DH-TMS directly (0 PG queries).
	r1 := doGet("/api/v1/search?q=direct%20movie")
	require.Equal(t, 200, r1.Code)
	require.Equal(t, int32(0), spy.v1Calls.Load())
	require.Contains(t, r1.Body.String(), tmsHash)

	// 2. /api/v2/search?q=... hits DH-TMS /v1/substring directly (0 PG queries).
	r2 := doGet("/api/v2/search?q=direct")
	require.Equal(t, 200, r2.Code)
	require.Equal(t, int32(0), spy.v2Calls.Load())
	require.Contains(t, r2.Body.String(), tmsHash)

	// 3. Lease expiry fails closed to PostgreSQL transparently on BOTH v1 and v2.
	expired.Store(true)
	r1fb := doGet("/api/v1/search?q=direct%20movie")
	require.Equal(t, 200, r1fb.Code)
	require.Equal(t, int32(1), spy.v1Calls.Load())
	require.Contains(t, r1fb.Body.String(), pgHash)

	r2fb := doGet("/api/v2/search?q=direct")
	require.Equal(t, 200, r2fb.Code)
	require.Equal(t, int32(1), spy.v2Calls.Load())
	require.Contains(t, r2fb.Body.String(), pgHash)

	// 4. PG-only filter (hdr=HDR10) bypasses DH-TMS cleanly even when lease is active.
	expired.Store(false)
	rBypass := doGet("/api/v1/search?q=direct&hdr=HDR10")
	require.Equal(t, 200, rBypass.Code)
	require.Equal(t, int32(2), spy.v1Calls.Load())

	// 5. video_source, video_codec, min_seeders serve directly from DH-TMS (0 additional PG calls),
	// while invalid enum values still fall through and return HTTP 400.
	rFiltered := doGet("/api/v1/search?q=direct&video_source=WEBDL&video_codec=x265&min_seeders=10")
	require.Equal(t, 200, rFiltered.Code)
	require.Equal(t, int32(2), spy.v1Calls.Load())

	rInvalid := doGet("/api/v1/search?video_source=VHS")
	require.Equal(t, 400, rInvalid.Code)
}
