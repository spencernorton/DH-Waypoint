package search

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/fts"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type bm25PostgresFixture struct {
	q    *dao.Query
	db   *gorm.DB
	pool *pgxpool.Pool
	cfg  *pgxpool.Config
}

// bm25TestDSN prefers the dedicated ParadeDB DSN and falls back to the
// shared PostgreSQL test DSN (which may itself point at ParadeDB).
func bm25TestDSN() string {
	if dsn := os.Getenv("DH_WAYPOINT_TEST_PARADEDB_DSN"); dsn != "" {
		return dsn
	}
	return os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
}

func newBM25PostgresFixture(t *testing.T) bm25PostgresFixture {
	t.Helper()
	dsn := bm25TestDSN()
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_PARADEDB_DSN or DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("dh_waypoint_bm25_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, cleanupErr := admin.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
CREATE TABLE torrent_contents (
  id text PRIMARY KEY, info_hash bytea NOT NULL,
  content_type text, content_source text, content_id text,
  languages jsonb, episodes jsonb,
  video_resolution text, video_source text, video_codec text,
  video_3d text, video_modifier text, release_group text,
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  tsv tsvector, seeders integer, leechers integer,
  published_at timestamptz NOT NULL DEFAULT now(), size bigint NOT NULL DEFAULT 4096,
  files_count integer, is_anime boolean NOT NULL DEFAULT false,
  release_granularity text, release_date date, anime_absolute_episode integer,
  english_audio text, english_audio_source text, release_attributes jsonb
)`)
	require.NoError(t, err)
	ResetDBCapabilitiesCache()
	t.Cleanup(ResetDBCapabilitiesCache)
	return bm25PostgresFixture{q: dao.Use(db), db: db, pool: pool, cfg: cfg}
}

// fresh returns handles on brand-new connections, simulating the application
// restart operators perform after enable/disable DDL so no connection holds
// a cached plan for the previous torrent_contents row shape.
func (f bm25PostgresFixture) fresh(t *testing.T) *search {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(context.Background(), f.cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sqlDB := stdlib.OpenDB(*f.cfg.ConnConfig)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	ResetDBCapabilitiesCache()
	return &search{q: dao.Use(db)}
}

func (f bm25PostgresFixture) insert(t *testing.T, id string, seed int, title, contentType, resolution string, episodes any, seeders int, published time.Time) protocol.InfoHash {
	t.Helper()
	var hash protocol.InfoHash
	for i := range 20 {
		hash[i] = byte(seed)
	}
	hash[0] = byte(seed >> 24)
	hash[1] = byte(seed >> 16)
	hash[2] = byte(seed >> 8)
	hash[3] = byte(seed)
	tsv := fts.Tsvector{}
	tsv.AddText(title, fts.TsvectorWeightA)
	var ct, res any
	if contentType != "" {
		ct = contentType
	}
	if resolution != "" {
		res = resolution
	}
	_, err := f.pool.Exec(context.Background(), `INSERT INTO torrent_contents
  (id, info_hash, content_type, video_resolution, episodes, tsv, seeders, published_at)
VALUES ($1, $2, $3, $4, $5, $6::tsvector, $7, $8)`,
		id, hash.Bytes(), ct, res, episodes, tsv.String(), seeders, published)
	require.NoError(t, err)
	return hash
}

func (f bm25PostgresFixture) contentsSnapshot(t *testing.T) string {
	t.Helper()
	var snapshot string
	err := f.pool.QueryRow(context.Background(), `SELECT COALESCE(string_agg(x::text, E'\n' ORDER BY x->>'id'), '')
FROM (SELECT to_jsonb(c)-'created_at'-'updated_at'-'seeders'-'leechers'-'published_at'-'tsv'-'search_text'-'ep_text' AS x FROM torrent_contents c) s`).Scan(&snapshot)
	require.NoError(t, err)
	return snapshot
}

func bm25ResultHashes(items []TorrentContentResultItem) []protocol.InfoHash {
	hashes := make([]protocol.InfoHash, 0, len(items))
	for _, item := range items {
		hashes = append(hashes, item.InfoHash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	return hashes
}

//nolint:gosmopolitan
func TestParadeDBBM25Postgres(t *testing.T) {
	f := newBM25PostgresFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	copperHash := f.insert(t, "bm25-movie-copper", 1, "Synthetic Copper Acacia 2026 1080p", "movie", "V1080p", nil, 50, now.AddDate(0, 0, -1))
	f.insert(t, "bm25-movie-bonsai", 2, "Synthetic Copper Bonsai 2026 720p", "movie", "V720p", nil, 5, now.AddDate(0, 0, -40))
	epHash := f.insert(t, "bm25-tv-ep", 3, "Synthetic Harbor Lights S02E05 1080p", "tv_show", "V1080p", `{"2":{"5":{}}}`, 30, now.AddDate(0, 0, -2))
	packHash := f.insert(t, "bm25-tv-pack", 4, "Synthetic Harbor Lights S02 Complete 1080p", "tv_show", "V1080p", `{"2":{}}`, 20, now.AddDate(0, 0, -3))
	otherEpHash := f.insert(t, "bm25-tv-ep6", 5, "Synthetic Harbor Lights S02E06 1080p", "tv_show", "V1080p", `{"2":{"6":{}}}`, 10, now.AddDate(0, 0, -4))
	cjkHash := f.insert(t, "bm25-cjk", 6, "Synthetic 明 1080p", "movie", "V1080p", nil, 7, now.AddDate(0, 0, -5))
	for i := range 300 {
		ct := "movie"
		if i%2 == 1 {
			ct = "tv_show"
		}
		f.insert(t, fmt.Sprintf("bm25-filler-%03d", i), 1000+i,
			fmt.Sprintf("Synthetic Filler Number %d 1080p", i), ct, "V1080p", nil, i%90, now.Add(-time.Duration(i)*time.Hour))
	}
	_, err := f.pool.Exec(ctx, `ANALYZE torrent_contents`)
	require.NoError(t, err)

	s := &search{q: f.q}
	searchInMode := func(t *testing.T, mode string, opts ...query.Option) TorrentContentResult {
		t.Helper()
		t.Setenv("SEARCH_BM25_MODE", mode)
		result, searchErr := s.TorrentContent(ctx, opts...)
		require.NoError(t, searchErr)
		return result
	}
	exact := func(q string, extra ...query.Option) []query.Option {
		return append(adultServingExactOptions(model.TableNameTorrentContent), append([]query.Option{query.SearchString(q)}, extra...)...)
	}
	parity := func(t *testing.T, name string, opts ...query.Option) TorrentContentResult {
		t.Helper()
		off := searchInMode(t, "off", opts...)
		on := searchInMode(t, "on", opts...)
		require.Equal(t, off.TotalCount, on.TotalCount, name)
		require.Equal(t, bm25ResultHashes(off.Items), bm25ResultHashes(on.Items), name)
		return off
	}

	status, err := ParadeDBBM25Status(ctx, f.db)
	require.NoError(t, err)
	if !status.ExtensionAvailable {
		// Stock PostgreSQL: BM25 management must refuse cleanly and search
		// must keep working on the tsquery path in every mode.
		require.Error(t, EnableParadeDBBM25(ctx, f.db))
		got := parity(t, "stock fallback", exact("synthetic copper")...)
		require.Contains(t, bm25ResultHashes(got.Items), copperHash)
		t.Skip("pg_search is not available on this PostgreSQL; skipping ParadeDB-specific subtests")
	}

	// Install the extension at database level so its objects land in the
	// default schema rather than the disposable fixture schema.
	_, err = f.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_search`)
	require.NoError(t, err)

	snapshotBefore := f.contentsSnapshot(t)
	require.NoError(t, EnableParadeDBBM25(ctx, f.db))
	require.NoError(t, EnableParadeDBBM25(ctx, f.db), "enable must be idempotent")

	status, err = ParadeDBBM25Status(ctx, f.db)
	require.NoError(t, err)
	require.True(t, status.ExtensionInstalled)
	require.NotEmpty(t, status.ExtensionVersion)
	require.True(t, status.SearchTextColumn && status.SearchTextGenerated, "search_text must be a generated column: %+v", status)
	require.True(t, status.EpisodeTextColumn && status.EpisodeTextGenerated, "ep_text must be a generated column: %+v", status)
	require.True(t, status.IndexValid)
	require.True(t, status.Active)
	require.True(t, HasParadeDBBM25(ctx, f.db))
	require.True(t, HasBM25EpisodeText(ctx, f.db))

	// The canonical source snapshot JSON (processors/source.go strip list)
	// must be byte-for-byte unchanged by the generated columns, while the
	// unstripped row proves the columns really exist now.
	require.Equal(t, snapshotBefore, f.contentsSnapshot(t))
	var unstripped string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT to_jsonb(c)::text FROM torrent_contents c WHERE c.id = 'bm25-tv-pack'`).Scan(&unstripped))
	require.Contains(t, unstripped, `"search_text"`)
	require.Contains(t, unstripped, `"ep_text": "s2all"`)
	var epTokens string
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT ep_text FROM torrent_contents WHERE id = 'bm25-tv-ep'`).Scan(&epTokens))
	require.Equal(t, "s2e5", epTokens)

	t.Run("parity eligible and filtered", func(t *testing.T) {
		res := parity(t, "single term", exact("copper")...)
		require.EqualValues(t, 2, res.TotalCount)
		parity(t, "multi term", exact("synthetic copper")...)
		parity(t, "broad 1080p", exact("1080p")...)
		parity(t, "synthetic 1080p", exact("synthetic 1080p")...)
		cjk := parity(t, "single CJK lexeme", exact("明")...)
		require.Contains(t, bm25ResultHashes(cjk.Items), cjkHash)
		parity(t, "content type filter", exact("synthetic", query.Where(TorrentContentTypeCriteria(model.ContentTypeMovie)))...)
		parity(t, "resolution filter", exact("synthetic", query.Where(query.DBCriteria{SQL: "torrent_contents.video_resolution = ?", Args: []interface{}{"V720p"}}))...)
		parity(t, "seeders filter", exact("synthetic", query.Where(query.DBCriteria{SQL: "torrent_contents.seeders > ?", Args: []interface{}{10}}))...)
		parity(t, "published filter", exact("synthetic", query.Where(query.DBCriteria{SQL: "torrent_contents.published_at > ?", Args: []interface{}{now.AddDate(0, 0, -10)}}))...)
		ep := parity(t, "episode filter", exact("harbor", query.Where(TorrentContentEpisodesCriteria(model.Episodes{2: {5: {}}})))...)
		require.ElementsMatch(t, []protocol.InfoHash{epHash, packHash}, bm25ResultHashes(ep.Items))
		season := parity(t, "season filter", exact("harbor", query.Where(TorrentContentEpisodesCriteria(model.Episodes{2: {}})))...)
		require.ElementsMatch(t, []protocol.InfoHash{epHash, packHash, otherEpHash}, bm25ResultHashes(season.Items))

		facetOpts := func() []query.Option {
			return append(adultServingExactOptions(model.TableNameTorrentContent),
				query.SearchString("synthetic"),
				query.WithFacet(TorrentContentTypeFacet(query.FacetIsAggregated(), query.FacetHasFilter(query.FacetFilter{"movie": {}}))))
		}
		off := searchInMode(t, "off", facetOpts()...)
		on := searchInMode(t, "on", facetOpts()...)
		require.Equal(t, off.TotalCount, on.TotalCount, "facet filtered count")
		require.Equal(t,
			off.Aggregations[TorrentContentTypeFacetKey].Items["movie"].Count,
			on.Aggregations[TorrentContentTypeFacetKey].Items["movie"].Count, "facet aggregation")
	})

	t.Run("fallback complex queries", func(t *testing.T) {
		for _, q := range []string{"copper | bonsai", "synthetic !copper", "coppe*", "\"copper acacia\"", "harbor s2", "harbor s02"} {
			off := searchInMode(t, "off", exact(q)...)
			on := searchInMode(t, "on", exact(q)...)
			require.Equal(t, adultServingContentHashes(off.Items), adultServingContentHashes(on.Items), q)
			require.Equal(t, off.TotalCount, on.TotalCount, q)
		}
	})

	t.Run("relevance ranking executes", func(t *testing.T) {
		rankOpts := func() []query.Option {
			return []query.Option{
				query.WithTotalCount(true), query.WithAggregationBudget(0),
				query.SearchString("synthetic 1080p"), query.OrderByQueryStringRank(), query.Limit(5),
			}
		}
		full := searchInMode(t, "off", exact("synthetic 1080p")...)
		fullSet := bm25ResultHashes(full.Items)
		for _, mode := range []string{"off", "on"} {
			got := searchInMode(t, mode, rankOpts()...)
			require.Len(t, got.Items, 5, mode)
			for _, h := range bm25ResultHashes(got.Items) {
				require.Contains(t, fullSet, h, mode)
			}
		}
	})

	t.Run("explain topk", func(t *testing.T) {
		checks, verifyErr := VerifyParadeDBBM25Plan(ctx, f.db)
		require.NoError(t, verifyErr)
		require.Len(t, checks, 3)
		for _, check := range checks {
			require.True(t, check.UsesBM25Scan, check.Name)
			require.True(t, check.UsesTopK, check.Name)
			require.NotContains(t, check.Plan, "Seq Scan on torrent_contents", check.Name)
			t.Logf("EXPLAIN %s:\n%s", check.Name, check.Plan)
		}
	})

	t.Run("injection-shaped queries stay parameterized", func(t *testing.T) {
		var rowsBefore int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM torrent_contents`).Scan(&rowsBefore))
		for _, q := range []string{"synthetic' 1080p", "1080p'; DROP TABLE torrent_contents; --", "synthetic &&& 1080p"} {
			parity(t, q, exact(q)...)
		}
		var rowsAfter int
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM torrent_contents`).Scan(&rowsAfter))
		require.Equal(t, rowsBefore, rowsAfter)
	})

	t.Run("disable restores stock path", func(t *testing.T) {
		require.NoError(t, DisableParadeDBBM25(ctx, f.db))
		disabled, statusErr := ParadeDBBM25Status(ctx, f.db)
		require.NoError(t, statusErr)
		require.False(t, disabled.Active)
		require.False(t, disabled.SearchTextColumn)
		require.False(t, disabled.IndexExists)
		require.False(t, HasParadeDBBM25(ctx, f.db))
		t.Setenv("SEARCH_BM25_MODE", "on")
		got, searchErr := f.fresh(t).TorrentContent(ctx, exact("synthetic copper")...)
		require.NoError(t, searchErr)
		require.Contains(t, bm25ResultHashes(got.Items), copperHash)
	})
}
