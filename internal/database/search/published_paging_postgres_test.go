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
	for _, direction := range []OrderDirection{OrderDirectionAscending, OrderDirectionDescending} {
		// Restrict to the two exact ties so a different ordering of unrelated
		// hashes cannot accidentally conceal an unspecified sibling order.
		options := append(adultServingExactOptions(model.TableNameTorrentContent),
			query.Where(TorrentContentInfoHashCriteria(f.hashes[4])),
			query.OrderBy(TorrentContentOrderByPublishedAt.Clauses(direction)...))
		full, err := s.TorrentContent(ctx, options...)
		require.NoError(t, err)
		require.Len(t, full.Items, 2)
		if direction == OrderDirectionAscending {
			require.Less(t, full.Items[0].ID, full.Items[1].ID)
		} else {
			require.Greater(t, full.Items[0].ID, full.Items[1].ID)
		}
		for i, item := range full.Items {
			pageOpts := append(append([]query.Option{}, options...), query.Offset(uint(i)), query.Limit(1), query.WithHasNextPage(true))
			page, err := s.TorrentContent(ctx, pageOpts...)
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			require.Equal(t, item.ID, page.Items[0].ID)
			require.Equal(t, i < len(full.Items)-1, page.HasNextPage)
		}
	}
}
