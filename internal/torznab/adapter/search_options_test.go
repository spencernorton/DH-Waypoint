package adapter

import (
	"context"
	"slices"
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

// SD and HD split every resolution, and no resolution at all, between them.
func TestResolutionSubcategoriesPartition(t *testing.T) {
	sd := map[model.VideoResolution]bool{
		model.VideoResolutionV360p: true, model.VideoResolutionV480p: true,
		model.VideoResolutionV540p: true, model.VideoResolutionV576p: true,
	}
	uhd := map[model.VideoResolution]bool{model.VideoResolutionV2160p: true, model.VideoResolutionV4320p: true}

	for _, c := range resolutionCategories {
		require.Equal(t, []int{c.sd}, resolutionSubcategories(c.parent, model.NullVideoResolution{}))

		for _, res := range model.VideoResolutionValues() {
			want := []int{c.hd}
			switch {
			case sd[res]:
				want = []int{c.sd}
			case uhd[res]:
				want = []int{c.hd, c.uhd}
			}
			require.Equal(t, want, resolutionSubcategories(c.parent, model.NewNullVideoResolution(res)), res)
		}
	}

	require.Empty(t, resolutionSubcategories(torznab.CategoryAudio.ID, model.NullVideoResolution{}))
}

func TestMergeResolutionCats(t *testing.T) {
	for _, tt := range []struct{ in, want []int }{
		{[]int{5030, 5040}, []int{5000}},
		{[]int{5040, 5070, 5030}, []int{5070, 5000}},
		{[]int{2030, 2040, 5030}, []int{5030, 2000}},
		{[]int{5030}, []int{5030}},
		{[]int{5040, 5045}, []int{5040, 5045}},
		{nil, nil},
	} {
		in := slices.Clone(tt.in)
		require.Equal(t, tt.want, mergeResolutionCats(tt.in))
		require.Equal(t, in, tt.in, "the request's categories must not be changed")
	}
}
