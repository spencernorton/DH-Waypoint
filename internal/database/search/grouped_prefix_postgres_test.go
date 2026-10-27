package search

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gen/field"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type groupedPrefixTrace struct {
	gormlogger.Interface
	prefixes atomic.Int64
	probes   atomic.Int64
}

func (l *groupedPrefixTrace) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	if strings.HasPrefix(strings.TrimSpace(sql), "WITH eligible AS NOT MATERIALIZED") {
		l.prefixes.Add(1)
	}
	if strings.HasPrefix(strings.TrimSpace(sql), "SELECT count(*) FROM (") && strings.Contains(sql, "bounded_matches") {
		l.probes.Add(1)
	}
}

func TestGroupedPrefixPostgresCanonicalJoinedPathExecutes(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	f, trace := observeGroupedPrefixFixture(t, f)
	result := requireGroupedPrefixEquivalent(t, f.serving(t, false),
		query.OrderBy(append(TorrentContentOrderByName.Clauses(OrderDirectionAscending),
			TorrentContentOrderByInfoHash.Clauses(OrderDirectionDescending)...)...),
		query.Limit(1), query.WithHasNextPage(true))
	require.Len(t, result.Items, 1)
	require.True(t, result.HasNextPage)
	require.Positive(t, trace.prefixes.Load(), "canonical raw-name join must execute the optimized SQL, not merely compare two fallback paths")
}

func TestGroupedPrefixPostgresTextAndLanguageStayEligible(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	_, err := f.pool.Exec(context.Background(), `
UPDATE torrent_contents SET search_string='Copper Acacia', languages='["en"]' WHERE info_hash=$1;
`, f.hashes[4].Bytes())
	require.NoError(t, err)
	_, err = f.pool.Exec(context.Background(),
		`UPDATE torrent_contents SET search_string='Copper Acacia', languages='["fr"]' WHERE info_hash=$1`, f.hashes[1].Bytes())
	require.NoError(t, err)
	f, trace := observeGroupedPrefixFixture(t, f)
	result := requireGroupedPrefixEquivalent(t, f.serving(t, false),
		query.SearchString("Copper Acacia"),
		query.WithFacet(TorrentContentLanguageFacet(query.FacetHasFilter(query.FacetFilter{"en": {}}))),
		query.OrderBy(TorrentContentOrderBySeeders.Clauses(OrderDirectionDescending)...),
		query.Limit(1), query.WithHasNextPage(true))
	require.EqualValues(t, 1, result.TotalCount)
	require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items))
	require.False(t, result.HasNextPage)
	require.Positive(t, trace.prefixes.Load(), "fast searched pages retain the prefix optimization")
}

func TestGroupedPrefixPostgresSlowTextPrefixFallsBackWithinCallerBudget(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	sqlDB, err := f.q.TorrentContent.WithContext(context.Background()).UnderlyingDB().DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	var jit string
	require.NoError(t, f.pool.QueryRow(context.Background(), "SHOW jit").Scan(&jit))
	require.Equal(t, "on", jit)
	_, err = f.pool.Exec(context.Background(), `
CREATE FUNCTION slow_prefix_only() RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
BEGIN PERFORM pg_sleep(10); RETURN true; END $$`)
	require.NoError(t, err)
	_, err = f.pool.Exec(context.Background(),
		`UPDATE torrent_contents SET search_string='Copper Acacia' WHERE info_hash=$1`, f.hashes[4].Bytes())
	require.NoError(t, err)
	f, trace := observeGroupedPrefixFixture(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	result, err := f.serving(t, false).TorrentContent(ctx,
		TorrentContentCoreJoins(), HydrateTorrentContentTorrent(),
		TorrentContentGroupByContentOption(),
		query.SearchString("Copper Acacia"),
		query.Where(query.DBCriteria{SQL: "(current_setting('jit') <> 'off' OR slow_prefix_only())"}),
		query.WithTotalCount(true), query.WithAggregationBudget(0),
		query.OrderBy(TorrentContentOrderBySeeders.Clauses(OrderDirectionDescending)...),
		query.Limit(1), query.WithHasNextPage(true))
	require.NoError(t, err, "the optional prefix must leave time for the authoritative grouped query")
	require.NoError(t, ctx.Err(), "the caller's deadline remains intact")
	require.EqualValues(t, 1, result.TotalCount)
	require.Equal(t, f.hashes[4:5], adultServingContentHashes(result.Items))
	require.False(t, result.HasNextPage)
	require.LessOrEqual(t, trace.probes.Load(), int64(1), "the cost hint is attempted only once")
	require.Equal(t, int64(1), trace.prefixes.Load(), "stop the slow prefix before expanding it again")
	require.NoError(t, f.q.TorrentContent.WithContext(context.Background()).UnderlyingDB().Raw("SHOW jit").Scan(&jit).Error)
	require.Equal(t, "on", jit, "canceled prefix settings cannot leak into pooled connections")
}

func observeGroupedPrefixFixture(t *testing.T, f adultServingPostgresFixture) (adultServingPostgresFixture, *groupedPrefixTrace) {
	t.Helper()
	trace := &groupedPrefixTrace{Interface: gormlogger.Discard}
	db := f.q.TorrentContent.WithContext(context.Background()).UnderlyingDB()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	observed, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: trace})
	require.NoError(t, err)
	f.q = dao.Use(observed)
	return f, trace
}

func originalContentGroupingOption() query.Option {
	return query.Options(
		query.GroupByDistinctOn(torrentContentGroupKeySQL, torrentContentGroupInnerOrderSQL),
		query.Where(query.DBCriteria{SQL: model.TableNameTorrentContent + ".content_id IS NOT NULL"}),
	)
}

func requireGroupedPrefixEquivalent(t *testing.T, s *search, options ...query.Option) TorrentContentResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := []query.Option{TorrentContentCoreJoins(), HydrateTorrentContentTorrent(),
		query.WithTotalCount(true), query.WithAggregationBudget(0)}
	find := func(grouping query.Option) TorrentContentResult {
		t.Helper()
		opts := append([]query.Option{grouping}, base...)
		opts = append(opts, options...)
		result, err := s.TorrentContent(ctx, opts...)
		require.NoError(t, err)
		return result
	}
	expected := find(originalContentGroupingOption())
	actual := find(TorrentContentGroupByContentOption())
	require.Equal(t, adultServingContentHashes(expected.Items), adultServingContentHashes(actual.Items))
	require.Equal(t, expected.TotalCount, actual.TotalCount)
	require.Equal(t, expected.HasNextPage, actual.HasNextPage)
	require.Equal(t, expected.Aggregations, actual.Aggregations)
	for i := range actual.Items {
		require.Equal(t, expected.Items[i].Torrent.Name, actual.Items[i].Torrent.Name, "raw torrent callback")
		require.Equal(t, expected.Items[i].Content.Title, actual.Items[i].Content.Title, "content callback")
	}
	return actual
}

func TestGroupedPrefixPostgresCustomSameTableJoinFallsBack(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	result := requireGroupedPrefixEquivalent(t, f.serving(t, false),
		query.Where(query.DBCriteria{SQL: "torrent_contents.info_hash=decode('" + f.hashes[4].String() + "','hex')"}),
		query.Join(func(q *dao.Query) []query.TableJoin {
			// A target table's primary key does not make a partial-key join
			// unique. The two movie metadata rows duplicate this release.
			return []query.TableJoin{{Table: q.Content,
				On:   []field.Expr{q.TorrentContent.ContentType.EqCol(q.Content.Type)},
				Type: query.TableJoinTypeInner, Required: true}}
		}),
		query.OrderBy(TorrentContentOrderByInfoHash.Clauses(OrderDirectionAscending)...),
		query.Limit(1), query.WithHasNextPage(true),
	)
	require.EqualValues(t, 1, result.TotalCount)
	require.Len(t, result.Items, 1)
	require.False(t, result.HasNextPage)
}

func TestGroupedPrefixPostgresRawScopeJoinFallsBack(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	f, trace := observeGroupedPrefixFixture(t, f)
	result := requireGroupedPrefixEquivalent(t, f.serving(t, false),
		query.Where(query.DBCriteria{SQL: "torrent_contents.info_hash=decode('" + f.hashes[4].String() + "','hex')"}),
		func(b query.OptionBuilder) (query.OptionBuilder, error) {
			return b.Scope(func(db *gorm.DB) error {
				joined := db.Joins("JOIN content duplicate_content ON duplicate_content.type=torrent_contents.content_type")
				db.Statement = joined.Statement
				return nil
			}), nil
		},
		query.OrderBy(TorrentContentOrderByInfoHash.Clauses(OrderDirectionAscending)...),
		query.Limit(1), query.WithHasNextPage(true))
	require.EqualValues(t, 1, result.TotalCount)
	require.Len(t, result.Items, 1)
	require.False(t, result.HasNextPage, "a raw many-row scope join cannot manufacture another group")
	require.Zero(t, trace.prefixes.Load(), "undeclared raw join must use the original path")
}

func TestGroupedPrefixPostgresRepresentativeOrderPagingAndHydration(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	// The ordinary representative has a tied eligible sibling whose raw name
	// sorts first. Hidden higher-seeder adult/quarantine siblings already live
	// in this fixture. NULL source groups exercise the separate winner branch.
	_, err := f.pool.Exec(ctx, `insert into torrent_contents
  (id,info_hash,content_type,content_source,content_id,seeders)
values ('synthetic-tie',$1,'movie','synthetic','ordinary',10),
       ('synthetic-null-1',$2,'movie',null,'null-title',5),
       ('synthetic-null-2',$3,'movie',null,'null-title',5)`,
		f.hashes[7].Bytes(), f.hashes[5].Bytes(), f.hashes[6].Bytes())
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `update torrents set name='Aardvark.Nonwinner.2031.mkv' where info_hash=$1`, f.hashes[7].Bytes())
	require.NoError(t, err)
	orders := []struct {
		name    string
		clauses []query.OrderByColumn
	}{
		{"raw name ascending", append(TorrentContentOrderByName.Clauses(OrderDirectionAscending), TorrentContentOrderByInfoHash.Clauses(OrderDirectionDescending)...)},
		{"seeders descending", TorrentContentOrderBySeeders.Clauses(OrderDirectionDescending)},
		{"seeders ascending", TorrentContentOrderBySeeders.Clauses(OrderDirectionAscending)},
	}
	before := f.snapshot(t)
	for _, adult := range []bool{false, true} {
		for _, order := range orders {
			for _, offset := range []uint{0, 1, 2, 3} {
				for _, limit := range []uint{1, 2} {
					t.Run(fmt.Sprintf("adult=%v/%s/offset=%d/limit=%d", adult, order.name, offset, limit), func(t *testing.T) {
						requireGroupedPrefixEquivalent(t, f.serving(t, adult), query.OrderBy(order.clauses...),
							query.Offset(offset), query.Limit(limit), query.WithHasNextPage(true))
					})
				}
			}
		}
	}
	result := requireGroupedPrefixEquivalent(t, f.serving(t, true),
		query.Where(query.DBCriteria{SQL: "torrent_contents.content_source IS NOT NULL"}),
		HydrateTorrentContentContent(),
		query.OrderBy(TorrentContentOrderBySeeders.Clauses(OrderDirectionDescending)...),
		query.Limit(10), query.WithHasNextPage(true))
	require.Equal(t, f.hashes[4], result.Items[0].InfoHash, "highest eligible seeder, then hash ascending, picks the original representative")
	require.Equal(t, "Copper Acacia", result.Items[0].Content.Title)
	require.Equal(t, before, f.snapshot(t), "all grouped paths and callbacks are read-only")
}

func TestGroupedPrefixPostgresShortageUsesExactWholeCorpusFallback(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	ctx := context.Background()
	_, err := f.pool.Exec(ctx, `delete from torrent_contents;
insert into torrents(info_hash,name)
select decode(lpad(to_hex(i+100000),40,'0'),'hex'),
       case when i=33000 then 'ZZZ.Synthetic.Winner' else 'AAA.Synthetic.'||lpad(i::text,5,'0') end
from generate_series(1,33000)i;
insert into torrent_contents(id,info_hash,content_type,content_source,content_id,seeders)
select 'synthetic-prefix-'||i,decode(lpad(to_hex(i+100000),40,'0'),'hex'),
       'movie','synthetic','ordinary',case when i=33000 then 2 else 1 end
from generate_series(1,33000)i;`)
	require.NoError(t, err)
	// The only representative lies after every allowed name prefix. A prefix
	// cap cannot prove exhaustion or substitute an earlier nonwinner row.
	result := requireGroupedPrefixEquivalent(t, f.serving(t, false),
		query.OrderBy(append(TorrentContentOrderByName.Clauses(OrderDirectionAscending),
			TorrentContentOrderByInfoHash.Clauses(OrderDirectionAscending)...)...),
		query.Limit(1), query.WithHasNextPage(true))
	require.EqualValues(t, 1, result.TotalCount)
	require.Len(t, result.Items, 1)
	require.False(t, result.HasNextPage)
	require.Equal(t, "ZZZ.Synthetic.Winner", result.Items[0].Torrent.Name)
}

func TestGroupedPrefixPostgresDefaultOrderKeepsCanonicalGroupPage(t *testing.T) {
	for _, offset := range []uint{0, 1, 2} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			f := newAdultServingPostgresFixture(t)
			f, trace := observeGroupedPrefixFixture(t, f)
			requireGroupedPrefixEquivalent(t, f.serving(t, true), query.Offset(offset), query.Limit(2), query.WithHasNextPage(true))
			require.Positive(t, trace.prefixes.Load())
		})
	}
}
