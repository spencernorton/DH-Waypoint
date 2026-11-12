package adapter

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
)

type identifierDispatchSearch struct {
	search.Search
	ordinary int
	resolved int
	ctx      context.Context
	refs     []model.ContentRef
}

func (s *identifierDispatchSearch) TorrentContent(ctx context.Context, _ ...query.Option) (search.TorrentContentResult, error) {
	s.ordinary++
	s.ctx = ctx
	return search.TorrentContentResult{}, nil
}

func (s *identifierDispatchSearch) TorrentContentByIdentifiers(ctx context.Context, refs []model.ContentRef, _ ...query.Option) (search.TorrentContentResult, error) {
	s.resolved++
	s.ctx = ctx
	s.refs = refs
	return search.TorrentContentResult{}, nil
}

// Hide the optional interface while preserving the ordinary Search contract.
type ordinaryDispatchSearch struct{ search.Search }

func TestAdapterDispatchesIdentifierSnapshotSearch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, tt := range []struct {
		name     string
		req      torznab.SearchRequest
		resolved bool
		refs     []model.ContentRef
	}{
		{"IMDb normalized", torznab.SearchRequest{Type: torznab.FunctionTV, IMDBID: model.NewNullString("123")}, true,
			[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt123"}}},
		{"typed TMDB", torznab.SearchRequest{Type: torznab.FunctionMovie, TMDBID: model.NewNullString("42")}, false, nil},
		{"untyped TMDB keeps namespaces", torznab.SearchRequest{Type: torznab.FunctionSearch, TMDBID: model.NewNullString("42")}, true,
			[]model.ContentRef{{Type: model.ContentTypeMovie, Source: "tmdb", ID: "42"}, {Type: model.ContentTypeTvShow, Source: "tmdb", ID: "42"}}},
		{"text", torznab.SearchRequest{Type: torznab.FunctionSearch}, false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &identifierDispatchSearch{}
			tt.req.Profile = torznab.ProfileDefault
			_, err := New(s).Search(ctx, tt.req)
			require.NoError(t, err)
			require.Same(t, ctx, s.ctx)
			require.Equal(t, tt.refs, s.refs)
			if tt.resolved {
				require.Equal(t, 1, s.resolved)
				require.Zero(t, s.ordinary)
			} else {
				require.Equal(t, 1, s.ordinary)
				require.Zero(t, s.resolved)
			}
		})
	}
	s := &identifierDispatchSearch{}
	_, err := New(ordinaryDispatchSearch{Search: s}).Search(ctx, torznab.SearchRequest{
		Type: torznab.FunctionTV, IMDBID: model.NewNullString("123"), Profile: torznab.ProfileDefault,
	})
	require.NoError(t, err)
	require.Equal(t, 1, s.ordinary)
	require.Zero(t, s.resolved)
}

type identifierWithUnlinkedDispatchSearch struct {
	identifierDispatchSearch
	unlinked int
	limit    uint
	offset   uint
}

func (s *identifierWithUnlinkedDispatchSearch) TorrentContentByIdentifiersWithUnlinked(
	ctx context.Context,
	refs []model.ContentRef,
	limit, offset uint,
	_ ...query.Option,
) (search.TorrentContentResult, error) {
	s.unlinked++
	s.ctx = ctx
	s.refs = refs
	s.limit = limit
	s.offset = offset
	return search.TorrentContentResult{}, nil
}

func TestAdapterDispatchesIdentifierWithUnlinkedSearch(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name     string
		req      torznab.SearchRequest
		unlinked bool
		refs     []model.ContentRef
	}{
		{"typed TMDB", torznab.SearchRequest{Type: torznab.FunctionTV, TMDBID: model.NewNullString("42")}, true,
			[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "42"}}},
		{"IMDb normalized", torznab.SearchRequest{Type: torznab.FunctionTV, IMDBID: model.NewNullString("123")}, true,
			[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt123"}}},
		{"TVDB mapped", torznab.SearchRequest{Type: torznab.FunctionTV, TVDBID: model.NewNullString("84")}, true,
			[]model.ContentRef{{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "84"}}},
		{"plain query without IDs", torznab.SearchRequest{Type: torznab.FunctionTV, Query: "lantern"}, false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &identifierWithUnlinkedDispatchSearch{}
			tt.req.Profile = torznab.ProfileDefault
			tt.req.Limit = model.NewNullUint(25)
			tt.req.Offset = model.NewNullUint(5)
			_, err := New(s).Search(ctx, tt.req)
			require.NoError(t, err)
			if tt.unlinked {
				require.Equal(t, 1, s.unlinked)
				require.Zero(t, s.resolved)
				require.Zero(t, s.ordinary)
				require.Equal(t, tt.refs, s.refs)
				require.Equal(t, uint(25), s.limit)
				require.Equal(t, uint(5), s.offset)
			} else {
				require.Equal(t, 1, s.ordinary)
				require.Zero(t, s.unlinked)
			}
		})
	}
}

type reverseReranker struct{}

func (reverseReranker) Rerank(_ context.Context, items []search.TorrentContentResultItem) []search.TorrentContentResultItem {
	out := make([]search.TorrentContentResultItem, len(items))
	for i := range items {
		out[len(items)-1-i] = items[i]
	}
	return out
}

func TestRerankPreservingLinkedFirst(t *testing.T) {
	items := []search.TorrentContentResultItem{
		{TorrentContent: model.TorrentContent{ID: "linked-a", ContentID: model.NewNullString("1")}},
		{TorrentContent: model.TorrentContent{ID: "linked-b", ContentID: model.NewNullString("1")}},
		{TorrentContent: model.TorrentContent{ID: "unlinked-a"}},
		{TorrentContent: model.TorrentContent{ID: "unlinked-b"}},
	}
	ranked := rerankPreservingLinkedFirst(context.Background(), reverseReranker{}, items, true)
	require.Equal(t, []string{"linked-b", "linked-a", "unlinked-b", "unlinked-a"}, []string{
		ranked[0].ID, ranked[1].ID, ranked[2].ID, ranked[3].ID,
	})
}
