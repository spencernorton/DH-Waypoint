package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/leanmode"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
)

type spyPGSearch struct {
	search.Search
	calls atomic.Int32
	items []search.TorrentContentResultItem
}

func (s *spyPGSearch) TorrentContent(_ context.Context, _ ...query.Option) (search.TorrentContentResult, error) {
	s.calls.Add(1)
	return search.TorrentContentResult{Items: s.items}, nil
}

func TestTorznabDirectServeAndFailClosedPGFallback(t *testing.T) {
	var expired atomic.Bool
	pubHash := strings.Repeat("a1", 20)
	unlinkedHash := strings.Repeat("a2", 20)
	pgHash := strings.Repeat("b1", 20)
	seeders := uint32(33)
	leechers := uint32(5)
	nowEpoch := uint32(time.Now().Unix())

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
					DocID: 1, InfoHashV1: pubHash, Name: "The.Usual.Suspects.1995.1080p.BluRay",
					SizeBytes: 8_000_000_000, ContentType: 1, Resolution: 6, Year: 1995,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch, Seeders: &seeders, Leechers: &leechers,
				}},
			})
		case "/v1/lookup":
			_ = json.NewEncoder(w).Encode(dhtms.LookupResponse{
				Linked: []dhtms.DocHit{{
					DocID: 1, InfoHashV1: pubHash, Name: "The.Usual.Suspects.1995.1080p.BluRay",
					SizeBytes: 8_000_000_000, ContentType: 1, Resolution: 6, Year: 1995,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch, Seeders: &seeders, Leechers: &leechers,
				}},
				Unlinked: []dhtms.DocHit{{
					DocID: 2, InfoHashV1: unlinkedHash, Name: "The.Usual.Suspects.1995.720p.WEBRip",
					SizeBytes: 3_000_000_000, ContentType: 0, Resolution: 5, Year: 1995,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch,
				}},
			})
		case "/v1/rss":
			_ = json.NewEncoder(w).Encode(dhtms.RSSResponse{
				Docs: []dhtms.DocHit{{
					DocID: 1, InfoHashV1: pubHash, Name: "The.Usual.Suspects.1995.1080p.BluRay",
					SizeBytes: 8_000_000_000, ContentType: 1, Resolution: 6,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch,
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
	spy := &spyPGSearch{
		items: []search.TorrentContentResultItem{{
			TorrentContent: model.TorrentContent{
				InfoHash:    pgIH,
				ContentType: model.NewNullContentType(model.ContentTypeMovie),
				PublishedAt: time.Now().UTC(),
				Size:        1000,
				Torrent: model.Torrent{
					InfoHash: pgIH,
					Name:     "PG.Fallback.Movie.2026.1080p",
					Size:     1000,
				},
			},
		}},
	}

	lean := leanmode.NewStore(nil)
	metrics := dhtms.NewMetrics()
	adp := New(spy).WithDHTMS(cli, nil, metrics, lean)

	// 1. Text search serves directly from DH-TMS (0 PG queries).
	res, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type:  torznab.FunctionSearch,
		Query: "usual suspects",
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load(), "direct DH-TMS serve must not query PG")
	require.Len(t, res.Channel.Items, 1)
	require.Equal(t, "The.Usual.Suspects.1995.1080p.BluRay", res.Channel.Items[0].Title)

	// 2. IMDB lookup returns linked + unlinked fallback directly from DH-TMS (0 PG queries).
	luRes, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type:   torznab.FunctionMovie,
		IMDBID: model.NewNullString("tt0114814"),
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Len(t, luRes.Channel.Items, 2)

	// 3. RSS browse serves directly from DH-TMS (0 PG queries).
	rssRes, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type: torznab.FunctionMovie,
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Len(t, rssRes.Channel.Items, 1)

	// 4. Fail-closed lease expiry (503) transparently falls back to PostgreSQL!
	expired.Store(true)
	fbRes, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type:  torznab.FunctionSearch,
		Query: "fallback",
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), spy.calls.Load(), "lease expiry must fall back to PG")
	require.Len(t, fbRes.Channel.Items, 1)
	require.Equal(t, "PG.Fallback.Movie.2026.1080p", fbRes.Channel.Items[0].Title)

	// 5. Global LEAN MODE bypasses DH-TMS and queries PostgreSQL directly.
	expired.Store(false)
	lean.SetActiveProfile(&leanmode.ProfileConfig{ID: "p1", Mode: leanmode.ModeServe}, true)
	leanRes, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type:  torznab.FunctionSearch,
		Query: "usual suspects",
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), spy.calls.Load(), "global LEAN MODE must bypass DH-TMS")
	require.Len(t, leanRes.Channel.Items, 1)
	require.Equal(t, "PG.Fallback.Movie.2026.1080p", leanRes.Channel.Items[0].Title)
}

func TestTorznabDHTMSServesProwlarrMultiCatAnd3DAnime(t *testing.T) {
	movie3DHash := strings.Repeat("11", 20)
	animeTVHash := strings.Repeat("22", 20)
	plainTVHash := strings.Repeat("33", 20)
	nowEpoch := uint32(time.Now().Unix())

	var lastRSSReq dhtms.RSSRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.Equal(t, "/v1/rss", r.URL.Path)
		lastRSSReq = dhtms.RSSRequest{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&lastRSSReq))
		docs := []dhtms.DocHit{
			{
				DocID: 1, InfoHashV1: movie3DHash, Name: "Avatar.2009.1080p.3D.SBS.BluRay",
				SizeBytes: 12_000_000_000, ContentType: 1, Resolution: 6, Video3D: 2, VideoSource: 9, VideoCodec: 3,
				PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch,
			},
			{
				DocID: 2, InfoHashV1: animeTVHash, Name: "[SubsPlease] Frieren - 01 (1080p)",
				SizeBytes: 1_500_000_000, ContentType: 2, Resolution: 6, IsAnime: true, VideoSource: 7, VideoCodec: 8,
				PublishedEpoch: nowEpoch - 10, CreatedEpoch: nowEpoch - 10,
			},
			{
				DocID: 3, InfoHashV1: plainTVHash, Name: "Ordinary.Show.S01E01.720p.WEBRip",
				SizeBytes: 900_000_000, ContentType: 2, Resolution: 5, IsAnime: false, VideoSource: 8, VideoCodec: 1,
				PublishedEpoch: nowEpoch - 20, CreatedEpoch: nowEpoch - 20,
			},
		}
		_ = json.NewEncoder(w).Encode(dhtms.RSSResponse{Docs: docs})
	}))
	defer srv.Close()

	cli, err := dhtms.NewClient(dhtms.Config{Mode: dhtms.ReadModeServe, Endpoint: srv.URL, Timeout: 2 * time.Second})
	require.NoError(t, err)
	defer cli.Close()

	spy := &spyPGSearch{}
	adp := New(spy).WithDHTMS(cli, nil, dhtms.NewMetrics(), nil)

	// 1. Prowlarr default Movies poll (2000,2040,2060) collapses to pure ct=1 with 0 PG queries!
	resMovies, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type: torznab.FunctionMovie,
		Cats: []int{2000, 2040, 2060},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.NotNil(t, lastRSSReq.ContentType)
	require.Equal(t, uint8(1), *lastRSSReq.ContentType)
	require.Empty(t, lastRSSReq.AnyClauses)
	require.Len(t, resMovies.Channel.Items, 1)

	// 2. Prowlarr default TV poll (5000,5040,5070) collapses to pure ct=2 with 0 PG queries!
	resTV, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type: torznab.FunctionTV,
		Cats: []int{5000, 5040, 5070},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.NotNil(t, lastRSSReq.ContentType)
	require.Equal(t, uint8(2), *lastRSSReq.ContentType)
	require.Empty(t, lastRSSReq.AnyClauses)
	require.Len(t, resTV.Channel.Items, 2)

	// 3. Dedicated cat=2060 (Movies/3D) sends require_3d clause and serves directly from DH-TMS!
	res3D, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type: torznab.FunctionSearch,
		Cats: []int{2060},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Len(t, lastRSSReq.AnyClauses, 1)
	require.True(t, lastRSSReq.AnyClauses[0].Require3D)
	require.Len(t, res3D.Channel.Items, 1)
	require.Equal(t, movie3DHash, res3D.Channel.Items[0].GUID)

	// 4. Dedicated cat=5070 (TV/Anime) sends require_anime clause, filters non-anime, and emits 5070 attr!
	resAnime, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type: torznab.FunctionSearch,
		Cats: []int{5070},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Len(t, lastRSSReq.AnyClauses, 1)
	require.True(t, lastRSSReq.AnyClauses[0].RequireAnime)
	require.Len(t, resAnime.Channel.Items, 1)
	require.Equal(t, animeTVHash, resAnime.Channel.Items[0].GUID)
	has5070 := false
	for _, a := range resAnime.Channel.Items[0].TorznabAttrs {
		if a.AttrName == torznab.AttrCategory && a.AttrValue == "5070" {
			has5070 = true
		}
	}
	require.True(t, has5070, "anime TV hit from DH-TMS must emit category 5070 attr")

	// 5. Mixed known + unknown category (2000,9999) ignores unknown cat 9999 instead of widening to all types.
	resMixed, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type: torznab.FunctionSearch,
		Cats: []int{2000, 9999},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.NotNil(t, lastRSSReq.ContentType)
	require.Equal(t, uint8(1), *lastRSSReq.ContentType)
	require.True(t, lastRSSReq.RequireV1)
	require.Len(t, resMixed.Channel.Items, 1)
	require.Equal(t, movie3DHash, resMixed.Channel.Items[0].GUID)

	// 6. Unknown-only category (9999) stays unconstrained across all types (matches PG).
	resUnknownOnly, err := adp.Search(context.Background(), torznab.SearchRequest{
		Type: torznab.FunctionSearch,
		Cats: []int{9999},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Nil(t, lastRSSReq.ContentType)
	require.Empty(t, lastRSSReq.AnyClauses)
	require.Len(t, resUnknownOnly.Channel.Items, 3)

	// 7. Deep search window (limit + offset + 1 > 500) bypasses DH-TMS to PG instead of truncating.
	_, err = adp.Search(context.Background(), torznab.SearchRequest{
		Profile: torznab.Profile{DefaultLimit: 50, MaxLimit: 100},
		Type:    torznab.FunctionSearch,
		Query:   "avatar",
		Limit:   model.NewNullUint(100),
		Offset:  model.NewNullUint(450),
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), spy.calls.Load(), "deep search offset > 500 must bypass DH-TMS to PG")
}

func TestTorznabDHTMSUnknownEntityBypassesToPG(t *testing.T) {
	var lastLookupReq dhtms.LookupRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.Equal(t, "/v1/lookup", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&lastLookupReq))
		_ = json.NewEncoder(w).Encode(dhtms.LookupResponse{EntityFound: false})
	}))
	defer srv.Close()

	cli, err := dhtms.NewClient(dhtms.Config{Mode: dhtms.ReadModeServe, Endpoint: srv.URL, Timeout: 2 * time.Second})
	require.NoError(t, err)
	defer cli.Close()

	spy := &spyPGSearch{}
	adp := New(spy).WithDHTMS(cli, nil, dhtms.NewMetrics(), nil)
	_, err = adp.Search(context.Background(), torznab.SearchRequest{
		Type:   torznab.FunctionMovie,
		IMDBID: model.NewNullString("tt9999999"),
	})
	require.NoError(t, err)
	require.True(t, lastLookupReq.RequireV1)
	require.Equal(t, int32(1), spy.calls.Load(), "unknown entity_found=false must bypass to PG")
}
