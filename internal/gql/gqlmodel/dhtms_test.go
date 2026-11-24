package gqlmodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	q "github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/gql/gqlmodel/gen"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/ast"
)

type spyGQLSearch struct {
	calls atomic.Int32
	items []search.TorrentContentResultItem
}

func (s *spyGQLSearch) TorrentContent(_ context.Context, _ ...q.Option) (search.TorrentContentResult, error) {
	s.calls.Add(1)
	return search.TorrentContentResult{Items: s.items, TotalCount: uint(len(s.items))}, nil
}

func gqlCtxWithFields(fields ...string) context.Context {
	sel := make(ast.SelectionSet, 0, len(fields))
	for _, name := range fields {
		sel = append(sel, &ast.Field{Name: name})
	}
	opCtx := &graphql.OperationContext{}
	fc := &graphql.FieldContext{
		Field: graphql.CollectedField{
			Field: &ast.Field{
				Name:         "search",
				SelectionSet: sel,
			},
			Selections: sel,
		},
	}
	ctx := graphql.WithOperationContext(context.Background(), opCtx)
	return graphql.WithFieldContext(ctx, fc)
}

func TestGraphQLTorrentContentSearchDHTMSAndBypass(t *testing.T) {
	var expired atomic.Bool
	tmsHash := strings.Repeat("e1", 20)
	pgHash := strings.Repeat("f2", 20)
	nowEpoch := uint32(time.Now().Unix())
	tot := uint64(42)
	seeders := uint32(19)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if expired.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"lease_expired","code":"lease_expired"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/search":
			_ = json.NewEncoder(w).Encode(dhtms.SearchResponse{
				Docs: []dhtms.DocHit{{
					DocID: 1, InfoHashV1: tmsHash, Name: "GraphQL.Hit.2026.1080p.WEBDL.x265",
					SizeBytes: 4_000_000_000, ContentType: 1, Resolution: 6, VideoSource: 7, VideoCodec: 3,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch, Seeders: &seeders,
				}},
				TotalFacets: &tot,
			})
		case "/v1/rss":
			_ = json.NewEncoder(w).Encode(dhtms.RSSResponse{
				Docs: []dhtms.DocHit{{
					DocID: 1, InfoHashV1: tmsHash, Name: "GraphQL.Hit.2026.1080p.WEBDL.x265",
					SizeBytes: 4_000_000_000, ContentType: 1, Resolution: 6, VideoSource: 7, VideoCodec: 3,
					PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch, Seeders: &seeders,
				}},
				TotalCount: &tot,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cli, err := dhtms.NewClient(dhtms.Config{Mode: dhtms.ReadModeServe, Endpoint: srv.URL, Timeout: 2 * time.Second})
	require.NoError(t, err)
	defer cli.Close()

	pgIH, err := protocol.ParseInfoHash(pgHash)
	require.NoError(t, err)
	spy := &spyGQLSearch{
		items: []search.TorrentContentResultItem{{
			TorrentContent: model.TorrentContent{
				InfoHash:    pgIH,
				ContentType: model.NewNullContentType(model.ContentTypeMovie),
				PublishedAt: time.Now().UTC(),
				Torrent:     model.Torrent{InfoHash: pgIH, Name: "PG.Fallback.GQL"},
			},
		}},
	}
	tcq := TorrentContentQuery{
		TorrentContentSearch: spy,
		DHTMS:                cli,
		DHTMSMetrics:         dhtms.NewMetrics(),
	}

	// 1. Eligible queryString search hits DH-TMS directly (0 PG calls).
	res, err := tcq.Search(gqlCtxWithFields("totalCount", "items"), TorrentContentSearchQueryInput{
		SearchParams: q.SearchParams{
			QueryString: model.NewNullString("1080p"),
			Limit:       model.NewNullUint(10),
			TotalCount:  model.NewNullBool(true),
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Equal(t, uint(42), res.TotalCount)
	require.Len(t, res.Items, 1)
	require.Equal(t, tmsHash, res.Items[0].InfoHash.String())
	require.Equal(t, model.VideoSourceWEBDL, res.Items[0].VideoSource.VideoSource)
	require.Equal(t, model.VideoCodecX265, res.Items[0].VideoCodec.VideoCodec)

	// 2. Eligible browse with ContentType filter hits DH-TMS /v1/rss directly (0 PG calls).
	movie := model.ContentTypeMovie
	resBrowse, err := tcq.Search(gqlCtxWithFields("totalCount", "items"), TorrentContentSearchQueryInput{
		SearchParams: q.SearchParams{
			Limit:      model.NewNullUint(10),
			TotalCount: model.NewNullBool(true),
		},
		Facets: &gen.TorrentContentFacetsInput{
			ContentType: graphql.OmittableOf(&gen.ContentTypeFacetInput{
				Filter: graphql.OmittableOf([]*model.ContentType{&movie}),
			}),
		},
	})
	require.NoError(t, err)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Equal(t, uint(42), resBrowse.TotalCount)

	// 3. Query selecting aggregations bypasses DH-TMS cleanly to PostgreSQL.
	resAgg, err := tcq.Search(gqlCtxWithFields("totalCount", "items", "aggregations"), TorrentContentSearchQueryInput{
		SearchParams: q.SearchParams{QueryString: model.NewNullString("1080p")},
	})
	require.NoError(t, err)
	require.Equal(t, int32(1), spy.calls.Load())
	require.Equal(t, pgHash, resAgg.Items[0].InfoHash.String())

	// 4. Lease expiry fails closed transparently to PostgreSQL.
	expired.Store(true)
	resFB, err := tcq.Search(gqlCtxWithFields("items"), TorrentContentSearchQueryInput{
		SearchParams: q.SearchParams{QueryString: model.NewNullString("1080p")},
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), spy.calls.Load())
	require.Equal(t, pgHash, resFB.Items[0].InfoHash.String())
}
