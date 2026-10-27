package search

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gen"
	"gorm.io/gorm"
)

func TestGroupedPrefixPostgresBroadTextStopsGuardingWhenThePageIsFull(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := f.pool.Exec(ctx, `
INSERT INTO torrents(info_hash,name,private)
SELECT decode(md5('stream-'||i)||'00000000','hex'),'Synthetic.Deep.mkv',i<2
FROM generate_series(0,39999) i;
INSERT INTO torrent_contents(id,info_hash,content_type,content_source,content_id,seeders,search_string)
SELECT 'stream-'||i,decode(md5('stream-'||i)||'00000000','hex'),
 'movie','synthetic','stream-'||i,40000-i,'Synthetic Deep'
FROM generate_series(0,39999) i;
UPDATE torrent_contents SET content_id='stream-shared' WHERE id IN ('stream-0','stream-2','stream-3');
UPDATE torrent_contents SET content_source=NULL WHERE id='stream-4';
CREATE INDEX ON torrent_contents((COALESCE(seeders,-1)) DESC,info_hash DESC)
 WHERE content_id IS NOT NULL;
CREATE INDEX ON torrent_contents(content_type,content_source,content_id,
 (COALESCE(seeders,-1)) DESC,info_hash);
ANALYZE torrents; ANALYZE torrent_contents;
CREATE FUNCTION witness_stream_eligibility(bytea) RETURNS boolean LANGUAGE plpgsql STABLE AS $$
BEGIN RAISE NOTICE 'stream-eligibility'; RETURN TRUE; END $$;`)
	require.NoError(t, err)
	var schema string
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema))
	cfg, err := pgx.ParseConfig(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	var evaluations atomic.Int64
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if n.Message == "stream-eligibility" {
			evaluations.Add(1)
		}
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)
	f.q = dao.Use(db)
	f, trace := observeGroupedPrefixFixture(t, f)
	run := func(group query.Option, offset uint) query.GenericResult[TorrentContentResultItem] {
		result, err := query.GenericQuery[TorrentContentResultItem](ctx, f.q,
			query.Options(group, query.WithTotalCount(false), query.SearchString("synthetic deep"),
				query.OrderBy(TorrentContentOrderBySeeders.Clauses(OrderDirectionDescending)...),
				query.Limit(2), query.Offset(offset), query.WithHasNextPage(true)), model.TableNameTorrentContent,
			func(ctx context.Context, q *dao.Query) query.SubQuery {
				eligible := q.TorrentContent.WithContext(ctx).ReadDB().Scopes(func(d gen.Dao) gen.Dao {
					v := d.(*gen.DO)
					v.ReplaceDB(v.UnderlyingDB().Where(
						"EXISTS(SELECT 1 FROM torrents t WHERE t.info_hash=torrent_contents.info_hash AND t.private=FALSE)").
						Where("witness_stream_eligibility(torrent_contents.info_hash)"))
					return d
				})
				return query.WithCandidatePrefixSource(query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: eligible},
					query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: q.TorrentContent.WithContext(ctx).ReadDB()})
			})
		require.NoError(t, err)
		return result
	}
	result := run(TorrentContentGroupByContentOption(), 0)
	require.Len(t, result.Items, 2)
	require.EqualValues(t, "stream-2", result.Items[0].ID)
	require.EqualValues(t, "stream-4", result.Items[1].ID)
	require.True(t, result.HasNextPage)
	require.Positive(t, trace.prefixes.Load())
	require.Positive(t, evaluations.Load())
	require.Less(t, evaluations.Load(), int64(32), "a broad page must stop checking candidates once enough guarded winners are found")
	baseline := run(originalContentGroupingOption(), 0)
	require.Equal(t, baseline.Items, result.Items)
	require.Equal(t, baseline.HasNextPage, result.HasNextPage)
	before := trace.prefixes.Load()
	deep := run(TorrentContentGroupByContentOption(), 4096)
	require.Greater(t, trace.prefixes.Load(), before, "a deep text page must attempt the capped prefix before fallback")
	require.EqualValues(t, "stream-4099", deep.Items[0].ID)
	require.EqualValues(t, "stream-4100", deep.Items[1].ID)
	baseline = run(originalContentGroupingOption(), 4096)
	require.Equal(t, baseline.Items, deep.Items)
	require.Equal(t, baseline.HasNextPage, deep.HasNextPage)
}
