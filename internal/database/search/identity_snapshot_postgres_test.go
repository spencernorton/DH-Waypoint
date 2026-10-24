package search

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestResolvedIdentitySnapshotRetainsAlternativesAndServing(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO content_attributes VALUES
 ('movie','synthetic','ordinary','imdb','id','tt-one')`)
	require.NoError(t, err)
	s := f.serving(t, true)
	ref := model.ContentRef{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt-one"}
	options := append(adultServingExactOptions(model.TableNameTorrentContent),
		TorrentContentDefaultHydrate(), query.WithFacet(TorrentContentTypeFacet(query.FacetIsAggregated())))
	result, err := s.TorrentContentByIdentifiers(ctx, []model.ContentRef{ref}, options...)
	require.NoError(t, err)
	require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items))
	require.Equal(t, "ordinary", result.Items[0].Content.ID)
	require.Equal(t, f.hashes[4], result.Items[0].Torrent.InfoHash)
	require.EqualValues(t, 1, result.Aggregations[TorrentContentTypeFacetKey].Items["movie"].Count)
	for _, miss := range []model.ContentRef{
		{Type: model.ContentTypeTvShow, Source: "imdb", ID: "tt-one"},
		{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt-one' OR true --"},
		{Type: model.ContentTypeMovie, Source: "tvdb", ID: "tt-one"},
	} {
		missing, lookupErr := s.TorrentContentByIdentifiers(ctx, []model.ContentRef{miss}, options...)
		require.NoError(t, lookupErr)
		require.Empty(t, missing.Items, "complete type/source/id namespaces and bound values must be retained")
	}
	// A missing alternate ID must not veto a mapped ID.
	refs := []model.ContentRef{ref, {Type: model.ContentTypeMovie, Source: "tvdb", ID: "missing"}}
	result, err = s.TorrentContentByIdentifiers(ctx, refs, options...)
	require.NoError(t, err)
	require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items))
	_, err = f.pool.Exec(ctx, `INSERT INTO content VALUES
 ('movie','synthetic','second','Synthetic second',false,now(),now())`)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO torrent_contents(id,info_hash,content_type,content_source,content_id)
 VALUES ('second-content',$1,'movie','synthetic','second')`, f.hashes[5].Bytes())
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO content_attributes VALUES
 ('movie','synthetic','second','imdb','id','tt-one')`)
	require.NoError(t, err)
	// Two distinct mappings retain OR semantics, with eligibility before paging.
	for offset := uint(0); offset < 2; offset++ {
		page := append(append([]query.Option(nil), options...), query.Offset(offset), query.Limit(1), query.WithHasNextPage(true))
		result, err = s.TorrentContentByIdentifiers(ctx, []model.ContentRef{ref}, page...)
		require.NoError(t, err)
		require.EqualValues(t, 2, result.TotalCount)
		require.EqualValues(t, 2, result.Aggregations[TorrentContentTypeFacetKey].Items["movie"].Count)
		require.Equal(t, f.hashes[4+offset:5+offset], adultServingContentHashes(result.Items))
		require.Equal(t, offset == 0, result.HasNextPage)
	}
}

func TestResolvedIdentityUsesOneReadSnapshot(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO content_attributes VALUES
 ('movie','synthetic','ordinary','imdb','id','tt-snapshot')`)
	require.NoError(t, err)
	changed := false
	db := f.q.Content.UnderlyingDB()
	name := "test:change-alias-after-resolution"
	require.NoError(t, db.Callback().Row().After("gorm:row").Register(name, func(tx *gorm.DB) {
		if changed || !strings.Contains(tx.Statement.SQL.String(), "identifier_keys") {
			return
		}
		changed = true
		_, changeErr := f.pool.Exec(ctx, `DELETE FROM content_attributes WHERE value='tt-snapshot'`)
		require.NoError(t, changeErr)
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Row().Remove(name)) })
	ref := model.ContentRef{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt-snapshot"}
	s := f.serving(t, true)
	options := adultServingExactOptions(model.TableNameTorrentContent)
	result, err := s.TorrentContentByIdentifiers(ctx, []model.ContentRef{ref}, options...)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items), "lookup and page must share the same snapshot")
	result, err = s.TorrentContentByIdentifiers(ctx, []model.ContentRef{ref}, options...)
	require.NoError(t, err)
	require.Empty(t, result.Items, "the next request must see the revoked alias")
}

func TestResolvedIdentityLookupHonorsCallerCancellation(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	lock, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Rollback(context.Background()) })
	_, err = lock.Exec(ctx, "LOCK TABLE content_attributes IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	requestCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = f.serving(t, true).TorrentContentByIdentifiers(requestCtx, []model.ContentRef{
		{Type: model.ContentTypeMovie, Source: "imdb", ID: "tt-blocked"},
	}, adultServingExactOptions(model.TableNameTorrentContent)...)
	require.Error(t, err)
	require.ErrorIs(t, requestCtx.Err(), context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second, "metadata lookup must inherit the caller deadline")
}
