package search

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
)

func TestContentIdentityFiltersBeforePagingAndRetainsServingPolicy(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO torrent_contents
  (id, info_hash, content_type, content_source, content_id, seeders) VALUES
  ('same-id-other-type', $1, 'tv_show', 'synthetic', 'ordinary', 20000),
  ('same-id-other-source', $2, 'movie', 'other', 'ordinary', 30000)`,
		f.hashes[5].Bytes(), f.hashes[6].Bytes())
	require.NoError(t, err)
	before := f.snapshot(t)
	s := f.serving(t, true)
	for _, offset := range []uint{0, 1} {
		options := append(adultServingExactOptions(model.TableNameTorrentContent),
			query.Where(TorrentContentIdentityCriteria(model.ContentRef{
				Type: model.ContentTypeMovie, Source: "synthetic", ID: "ordinary",
			})), query.Limit(1), query.Offset(offset), query.WithHasNextPage(true))
		result, err := s.TorrentContent(ctx, options...)
		require.NoError(t, err)
		require.EqualValues(t, 1, result.TotalCount)
		require.False(t, result.HasNextPage)
		if offset == 0 {
			require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items),
				"identity search must still exclude adult and quarantined siblings")
		} else {
			require.Empty(t, result.Items)
		}
	}
	result, err := s.TorrentContent(ctx, query.Where(TorrentContentIdentityCriteria(model.ContentRef{
		Type: model.ContentTypeMovie, Source: "synthetic", ID: "' OR true --",
	})))
	require.NoError(t, err)
	require.Empty(t, result.Items, "identity values must remain bound SQL parameters")
	require.Equal(t, before, f.snapshot(t))
}

func TestExternalIdentityUsesOnlyIdentifierAttributesAndServingPolicy(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO content_attributes
 (content_type,content_source,content_id,source,key,value) VALUES
 ('movie','synthetic','ordinary','imdb','id','tt123456'),
 ('movie','synthetic','adult','imdb','id','tt123456'),
 ('movie','synthetic','ordinary','tvdb','id','123456'),
 ('movie','synthetic','ordinary','imdb','alt_title:0','tt999999')`)
	require.NoError(t, err)
	s := f.serving(t, true)
	for _, tt := range []struct {
		source, id string
		kind       model.ContentType
		want       int
	}{
		{"imdb", "tt123456", model.ContentTypeMovie, 1},
		{"imdb", "tt123456", model.ContentTypeTvShow, 0},
		{"imdb", "tt999999", model.ContentTypeMovie, 0},
		{"tvdb", "tt123456", model.ContentTypeMovie, 0},
		{"imdb", "' OR true --", model.ContentTypeMovie, 0},
	} {
		options := append(adultServingExactOptions(model.TableNameTorrentContent),
			query.Where(TorrentContentIdentifiersCriteria(model.ContentRef{
				Type: tt.kind, Source: tt.source, ID: tt.id,
			})), query.Limit(1), query.WithHasNextPage(true))
		result, err := s.TorrentContent(ctx, options...)
		require.NoError(t, err)
		require.Len(t, result.Items, tt.want)
		if tt.want > 0 {
			require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items))
		}
	}
}
