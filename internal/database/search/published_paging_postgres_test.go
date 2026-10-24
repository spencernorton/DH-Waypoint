package search

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
)

func TestPublishedPagingOrdersContentSiblings(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `INSERT INTO content(type,source,id,title,adult)
 VALUES ('movie','synthetic','sibling','Synthetic sibling',false)`)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `INSERT INTO torrent_contents(id,info_hash,content_type,content_source,content_id)
 VALUES ('z-sibling',$1,'movie','synthetic','sibling')`, f.hashes[4].Bytes())
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `UPDATE torrent_contents SET published_at='2026-01-01'::timestamptz`)
	require.NoError(t, err)
	s := f.serving(t, true)
	options := append(adultServingExactOptions(model.TableNameTorrentContent),
		query.OrderBy(TorrentContentOrderByPublishedAt.Clauses(OrderDirectionDescending)...))
	full, err := s.TorrentContent(ctx, options...)
	require.NoError(t, err)
	require.Len(t, full.Items, 4)
	var siblingIDs []string
	for _, item := range full.Items {
		if item.InfoHash == f.hashes[4] {
			siblingIDs = append(siblingIDs, item.ID)
		}
	}
	require.Len(t, siblingIDs, 2)
	require.Greater(t, siblingIDs[0], siblingIDs[1], "equal published_at/hash needs the unique row ID tiebreaker")
	for i, item := range full.Items {
		pageOpts := append(append([]query.Option{}, options...), query.Offset(uint(i)), query.Limit(1), query.WithHasNextPage(true))
		page, err := s.TorrentContent(ctx, pageOpts...)
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, item.ID, page.Items[0].ID)
		require.Equal(t, i < len(full.Items)-1, page.HasNextPage)
	}
}
