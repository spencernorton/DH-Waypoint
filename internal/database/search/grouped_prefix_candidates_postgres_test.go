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

func TestGroupedPrefixPostgresBoundsEligibilityWorkBeforeReturningAPage(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := f.pool.Exec(ctx, `
INSERT INTO torrents(info_hash,name,private)
SELECT decode(md5('prefix-'||i)||'00000000','hex'),
       CASE WHEN i%7=0 THEN 'Synthetic.Siterip.mkv' ELSE 'Synthetic.Film.mkv' END,
       i%5=0 FROM generate_series(0,5999) i;
INSERT INTO torrent_contents(id,info_hash,content_type,content_source,content_id,seeders)
SELECT 'prefix-'||i,decode(md5('prefix-'||i)||'00000000','hex'),
       'movie','synthetic','prefix-'||i,6000-i FROM generate_series(0,5999) i;
CREATE INDEX ON torrent_contents(content_type,content_source,content_id,
  (COALESCE(seeders,-1)) DESC,info_hash);
ANALYZE torrents; ANALYZE torrent_contents;
CREATE FUNCTION witness_eligibility(bytea) RETURNS boolean LANGUAGE plpgsql STABLE AS $$
BEGIN RAISE NOTICE 'eligibility-evaluation'; RETURN TRUE; END $$;`)
	require.NoError(t, err)

	// A server notice observes predicate evaluation without modifying data in
	// the read-only prefix transaction or depending on wall-clock timings.
	var evaluations atomic.Int64
	var schema string
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema))
	cfg, err := pgx.ParseConfig(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	cfg.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
		if notice.Message == "eligibility-evaluation" {
			evaluations.Add(1)
		}
	}
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)
	f.q = dao.Use(db)
	f, trace := observeGroupedPrefixFixture(t, f)
	policy := f.serving(t, true).adultPolicy
	result, err := query.GenericQuery[TorrentContentResultItem](ctx, f.q,
		query.Options(TorrentContentGroupByContentOption(), query.WithTotalCount(false),
			query.OrderBy(TorrentContentOrderBySeeders.Clauses(OrderDirectionDescending)...),
			query.Limit(2), query.WithHasNextPage(true)),
		model.TableNameTorrentContent,
		func(ctx context.Context, q *dao.Query) query.SubQuery {
			base := q.TorrentContent.WithContext(ctx).ReadDB().Scopes(func(d gen.Dao) gen.Dao {
				do := d.(*gen.DO)
				do.ReplaceDB(do.UnderlyingDB().Where(policy.TorrentCondition(model.TableNameTorrentContent)).
					Where("witness_eligibility(torrent_contents.info_hash)"))
				return d
			})
			return query.WithCandidatePrefixSource(
				query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: base},
				query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: q.TorrentContent.WithContext(ctx).ReadDB()})
		})
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	require.EqualValues(t, "prefix-1", result.Items[0].ID)
	require.EqualValues(t, "prefix-2", result.Items[1].ID)
	require.True(t, result.HasNextPage)
	require.Positive(t, trace.prefixes.Load(), "exercise the prefix, not a complete-query fallback")
	require.Positive(t, evaluations.Load(), "the authoritative guard must execute")
	require.Less(t, evaluations.Load(), int64(1024), "a small page must not evaluate eligibility across all6000 matches")
}

func TestGroupedPrefixPostgresDifferentCandidateRootFallsBack(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	f, trace := observeGroupedPrefixFixture(t, f)
	ctx := context.Background()
	policy := f.serving(t, true).adultPolicy
	result, err := query.GenericQuery[TorrentContentResultItem](ctx, f.q,
		query.Options(TorrentContentGroupByContentOption(), query.WithTotalCount(false),
			query.OrderBy(TorrentContentOrderBySeeders.Clauses(OrderDirectionDescending)...),
			query.Limit(1), query.WithHasNextPage(true)), model.TableNameTorrentContent,
		func(ctx context.Context, q *dao.Query) query.SubQuery {
			base := q.TorrentContent.WithContext(ctx).ReadDB().Scopes(func(d gen.Dao) gen.Dao {
				do := d.(*gen.DO)
				do.ReplaceDB(do.UnderlyingDB().Where(policy.TorrentCondition(model.TableNameTorrentContent)))
				return d
			})
			other := q.TorrentContent.WithContext(ctx).ReadDB().Scopes(func(d gen.Dao) gen.Dao {
				do := d.(*gen.DO)
				do.ReplaceDB(do.UnderlyingDB().Table("synthetic_other_source"))
				return d
			})
			return query.WithCandidatePrefixSource(query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: base},
				query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: other})
		})
	require.NoError(t, err, "an incompatible hint must never query a different source")
	require.Len(t, result.Items, 1)
	require.Equal(t, f.hashes[4], result.Items[0].InfoHash)
	require.False(t, result.HasNextPage)
	require.Zero(t, trace.prefixes.Load(), "fall back to the authoritative grouped query")
}
