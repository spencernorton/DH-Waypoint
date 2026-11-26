package search

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestPayloadGatePrefixEscalationAndFailClosed(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)

	schema := fmt.Sprintf("dh_waypoint_payload_prefix_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = admin.ExecContext(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
	})

	cfg.RuntimeParams["search_path"] = schema + ",public"
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = sqlDB.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)

	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)

	procArtifact := strings.Repeat("a", 64)
	payloadArtifact := strings.Repeat("b", 64)
	_, err = pool.Exec(ctx, `INSERT INTO content (type, source, id, title, adult, created_at, updated_at)
		VALUES ('tv_show','tmdb','100','Synthetic series',false,now(),now())`)
	require.NoError(t, err)
	procIdentity := fmt.Sprintf(`{"artifact_digest":"%s","name":"test-reviewer","version":"1.0.0"}`, procArtifact)
	_, err = pool.Exec(ctx, `
INSERT INTO processor_registry (id, generation, execution_generation, endpoint, mode, state, priority, manifest, configuration)
VALUES ('test-reviewer', 1, 1, 'http://synthetic.invalid', 'apply', 'active', 1, jsonb_build_object('processor', $1::jsonb, 'capabilities', jsonb_build_array('policy')), '{}')`, procIdentity)
	require.NoError(t, err)

	// Insert 180 payload-verified TV rows ordered newest -> oldest:
	// - Rows 1..145 (newest): payload-approved, but MISSING processor_publication_reviews
	//   (must force prefix escalation 128 -> 512 and NEVER leak through Step 2).
	// - Row 146: payload+processor approved, but marked private=true (must be rejected).
	// - Row 147: payload+processor approved, but held in processor_publication_holds (must be rejected).
	// - Rows 148..180 (33 rows): completely eligible (must appear in exact published_at DESC order).
	baseTime := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var expectedEligible []string
	for i := 1; i <= 180; i++ {
		h := make([]byte, 20)
		h[0] = 0x55
		h[18] = byte(i >> 8)
		h[19] = byte(i)
		pubAt := baseTime.Add(-time.Duration(i) * time.Minute)
		isPrivate := i == 146
		name := fmt.Sprintf("Synthetic.Show.S01E%03d.1080p.WEB-DL.mkv", i)
		_, err = pool.Exec(ctx, `INSERT INTO torrents (info_hash, name, size, private, files_status, created_at, updated_at)
VALUES ($1, $2, 1000000, $3, 'single', $4, $4)`, h, name, isPrivate, pubAt)
		require.NoError(t, err)
		var tcID string
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO torrent_contents (info_hash, content_type, content_source, content_id, published_at, tsv, created_at, updated_at)
VALUES ($1, 'tv_show', 'tmdb', '100', $2, to_tsvector('simple', $3), $2, $2) RETURNING id`, h, pubAt, fmt.Sprintf("synthetic show s01e%03d 1080p", i)).Scan(&tcID))

		var rev int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash = $1`, h).Scan(&rev))
		_, err = pool.Exec(ctx, `INSERT INTO processor_payload_reviews (info_hash, source_revision, verifier_artifact, policy, approved, reason, receipt, expires_at)
VALUES ($1, $2, $3, 'piece-bound-media-format-v2', true, 'decoded_media', '{}'::jsonb, now() + interval '7 days')`, h, rev, payloadArtifact)
		require.NoError(t, err)

		if i >= 146 {
			_, err = pool.Exec(ctx, `INSERT INTO processor_publication_reviews (info_hash, processor_id, source_revision, execution_generation, processor_identity, approved, policy_contract, reason, job_id)
VALUES ($1, 'test-reviewer', $2, 1, $3::jsonb, true, 'core-publication-policy-v1', 'ok', $4)`, h, rev, procIdentity, fmt.Sprintf("job-%d", i))
			require.NoError(t, err)
		}
		if i == 147 {
			_, err = pool.Exec(ctx, `INSERT INTO processor_publication_holds (info_hash, held, reason, request_id) VALUES ($1, true, 'test-hold', repeat('c', 64))`, h)
			require.NoError(t, err)
		}
		if i >= 148 {
			expectedEligible = append(expectedEligible, tcID)
		}
	}

	policy, err := serving.NewPolicy(serving.Config{
		ExcludeAdult:                          true,
		RequiredProcessor:                     "test-reviewer",
		RequiredProcessorArtifactDigest:       procArtifact,
		RequiredPayloadVerifierArtifactDigest: payloadArtifact,
	}, []string{"syntheticadult"}, []string{"mkv"})
	require.NoError(t, err)

	s := search{q: dao.Use(gdb), adultPolicy: policy}

	// Every row has payload evidence, but the early members of this group
	// lack publication approval, are private, or are held. Both the browse
	// and the searched group must pick the first fully eligible hash.
	for _, term := range []string{"", "synthetic"} {
		opts := []query.Option{TorrentContentDefaultOption(), TorrentContentGroupByContentOption(), query.Limit(5)}
		if term != "" {
			opts = append(opts, query.SearchString(term))
		}
		group, groupErr := s.TorrentContent(ctx, opts...)
		require.NoError(t, groupErr)
		require.Len(t, group.Items, 1)
		require.Equal(t, expectedEligible[0], group.Items[0].ID)
	}

	// Page 1: limit=10, offset=0 (must escalate 128 -> 512 and return rows 148..157 with HasNextPage=true)
	res1, err := s.TorrentContent(ctx,
		TorrentContentDefaultOption(),
		query.Where(TorrentContentTypeCriteria(model.ContentTypeTvShow)),
		query.Limit(10),
		query.Offset(0),
		query.WithHasNextPage(true),
	)
	require.NoError(t, err)
	require.True(t, res1.HasNextPage)
	require.Len(t, res1.Items, 10)
	for idx, item := range res1.Items {
		require.Equal(t, expectedEligible[idx], item.ID)
	}

	// Page 2: limit=10, offset=25 (last 8 rows: 173..180, HasNextPage=false)
	res2, err := s.TorrentContent(ctx,
		TorrentContentDefaultOption(),
		query.Where(TorrentContentTypeCriteria(model.ContentTypeTvShow)),
		query.Limit(10),
		query.Offset(25),
		query.WithHasNextPage(true),
	)
	require.NoError(t, err)
	require.False(t, res2.HasNextPage)
	require.Len(t, res2.Items, 8)
	for idx, item := range res2.Items {
		require.Equal(t, expectedEligible[25+idx], item.ID)
	}

	// Text relevance search: matches only eligible row 155
	res3, err := s.TorrentContent(ctx,
		TorrentContentDefaultOption(),
		query.SearchString("s01e155"),
		query.OrderBy(TorrentContentOrderByRelevance.Clauses(OrderDirectionDescending)...),
		query.Limit(10),
		query.WithHasNextPage(true),
	)
	require.NoError(t, err)
	require.False(t, res3.HasNextPage)
	require.Len(t, res3.Items, 1)
	require.Equal(t, expectedEligible[155-148], res3.Items[0].ID)

	// Text relevance search matching ONLY an unapproved row (10) -> must return 0 rows (fail-closed)
	res4, err := s.TorrentContent(ctx,
		TorrentContentDefaultOption(),
		query.SearchString("s01e010"),
		query.OrderBy(TorrentContentOrderByRelevance.Clauses(OrderDirectionDescending)...),
		query.Limit(10),
		query.WithHasNextPage(true),
	)
	require.NoError(t, err)
	require.False(t, res4.HasNextPage)
	require.Empty(t, res4.Items)
}
