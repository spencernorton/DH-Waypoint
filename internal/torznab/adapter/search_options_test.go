package adapter

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
)

// The dual-ID tvsearch shape from the catalogue cliff: Sonarr-style clients
// send tmdbid and tvdbid together. Both identifiers must reach the snapshot
// identifier search as alternatives (OR), in one call, so they can be
// resolved to canonical keys inside one read snapshot.
func TestRequestIdentifierRefsDualID(t *testing.T) {
	req := torznab.SearchRequest{
		Type:   torznab.FunctionTV,
		TMDBID: model.NewNullString("119051"),
		TVDBID: model.NewNullString("83268"),
	}
	require.Equal(t, []model.ContentRef{
		{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "119051"},
		{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "83268"},
	}, requestIdentifierRefs(req))
}

func TestRequestIdentifierRefsAllSystemsHaveNoDuplicates(t *testing.T) {
	req := torznab.SearchRequest{
		Type:   torznab.FunctionSearch,
		IMDBID: model.NewNullString("123"),
		TMDBID: model.NewNullString("42"),
		TVDBID: model.NewNullString("84"),
	}
	refs := requestIdentifierRefs(req)
	require.Equal(t, []model.ContentRef{
		{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt123"},
		{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt123"},
		{Type: model.ContentTypeMovie, Source: "tmdb", ID: "42"},
		{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "42"},
		{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "84"},
	}, refs)
	seen := make(map[model.ContentRef]struct{}, len(refs))
	for _, ref := range refs {
		_, dup := seen[ref]
		require.False(t, dup, "duplicate identifier ref %v", ref)
		seen[ref] = struct{}{}
	}
}

func TestAdapterDispatchesDualIDToIdentifierSnapshotSearch(t *testing.T) {
	s := &identifierDispatchSearch{}
	req := torznab.SearchRequest{
		Type:    torznab.FunctionTV,
		TMDBID:  model.NewNullString("119051"),
		TVDBID:  model.NewNullString("83268"),
		Season:  model.NewNullInt(1),
		Episode: model.NewNullInt(1),
		Profile: torznab.ProfileDefault,
	}
	_, err := New(s).Search(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, 1, s.resolved, "dual-ID requests must use the snapshot identifier search, not the ordinary search")
	require.Zero(t, s.ordinary)
	require.Equal(t, []model.ContentRef{
		{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "119051"},
		{Type: model.ContentTypeTvShow, Source: "tvdb", ID: "83268"},
	}, s.refs)
}
