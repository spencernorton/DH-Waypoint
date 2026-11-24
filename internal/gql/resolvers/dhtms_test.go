package resolvers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	q "github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/gql"
	"github.com/spencernorton/DH-Waypoint/internal/gql/resolvers"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

type spySearch struct {
	search.Search
	calls atomic.Int32
	items []search.TorrentContentResultItem
}

func (s *spySearch) TorrentContent(_ context.Context, _ ...q.Option) (search.TorrentContentResult, error) {
	s.calls.Add(1)
	return search.TorrentContentResult{Items: s.items, TotalCount: uint(len(s.items))}, nil
}

func TestGraphQLExecutableSchemaDHTMSRoutingAndSelectionBypass(t *testing.T) {
	tmsHash := strings.Repeat("1a", 20)
	pgHash := strings.Repeat("2b", 20)
	nowEpoch := uint32(time.Now().Unix())
	tot := uint64(7)
	seeders := uint32(15)

	tmsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dhtms.SearchResponse{
			Docs: []dhtms.DocHit{{
				DocID: 1, InfoHashV1: tmsHash, Name: "Live.Schema.1080p.BluRay.x265",
				SizeBytes: 5_000_000_000, ContentType: 1, Resolution: 6, VideoSource: 9, VideoCodec: 3,
				PublishedEpoch: nowEpoch, CreatedEpoch: nowEpoch, Seeders: &seeders,
			}},
			TotalFacets: &tot,
		})
	}))
	defer tmsSrv.Close()

	cli, err := dhtms.NewClient(dhtms.Config{Mode: dhtms.ReadModeServe, Endpoint: tmsSrv.URL, Timeout: 2 * time.Second})
	require.NoError(t, err)
	defer cli.Close()

	pgIH, err := protocol.ParseInfoHash(pgHash)
	require.NoError(t, err)
	spy := &spySearch{
		items: []search.TorrentContentResultItem{{
			TorrentContent: model.TorrentContent{
				InfoHash:    pgIH,
				ContentType: model.NewNullContentType(model.ContentTypeMovie),
				PublishedAt: time.Now().UTC(),
				Torrent:     model.Torrent{InfoHash: pgIH, Name: "PG.UI.Row"},
			},
		}},
	}
	es := gql.NewExecutableSchema(gql.Config{
		Resolvers: &resolvers.Resolver{
			Search:       spy,
			DHTMSClient:  cli,
			DHTMSMetrics: dhtms.NewMetrics(),
		},
	})
	gqlSrv := handler.NewDefaultServer(es)

	execGQL := func(queryStr string) string {
		body, _ := json.Marshal(map[string]any{"query": queryStr})
		req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		gqlSrv.ServeHTTP(rec, req)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return rec.Body.String()
	}

	// 1. Eligible query with torrent subfields hits DH-TMS directly (0 PG queries).
	out1 := execGQL(`{ torrentContent { search(input: { queryString: "1080p", limit: 5, totalCount: true }) { totalCount items { infoHash contentType title videoResolution videoSource videoCodec seeders torrent { name size magnetUri } } } } }`)
	require.Equal(t, int32(0), spy.calls.Load())
	require.Contains(t, out1, tmsHash)
	require.Contains(t, out1, `"totalCount":7`)

	// 2. UI query selecting aggregations bypasses DH-TMS to PG.
	out2 := execGQL(`{ torrentContent { search(input: { queryString: "1080p", limit: 5 }) { items { infoHash } aggregations { contentType { label count } } } } }`)
	require.Equal(t, int32(1), spy.calls.Load())
	require.Contains(t, out2, pgHash)

	// 3. UI query selecting content relation bypasses DH-TMS to PG.
	out3 := execGQL(`{ torrentContent { search(input: { queryString: "1080p", limit: 5 }) { items { infoHash content { title } } } } }`)
	require.Equal(t, int32(2), spy.calls.Load())
	require.Contains(t, out3, pgHash)

	// 4. UI query selecting torrent.files bypasses DH-TMS to PG.
	out4 := execGQL(`{ torrentContent { search(input: { queryString: "1080p", limit: 5 }) { items { infoHash torrent { name files { path } } } } } }`)
	require.Equal(t, int32(3), spy.calls.Load())
	require.Contains(t, out4, pgHash)
}
