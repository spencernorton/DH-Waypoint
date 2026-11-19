package torrentmetrics

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencernorton/DH-Waypoint/internal/metrics"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newTorrentMetricsTestDB(t *testing.T) (*pgxpool.Pool, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("torrentmetrics_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
CREATE TABLE torrents_torrent_sources (
	source text NOT NULL,
	info_hash bytea NOT NULL,
	created_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (source, info_hash)
);
CREATE INDEX idx_tts_updated_at ON torrents_torrent_sources (updated_at);
`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		if c, e := pgx.Connect(context.Background(), dsn); e == nil {
			_, _ = c.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
			_ = c.Close(context.Background())
		}
	})
	return pool, gdb
}

// Synthetic many-bucket regression verifying omitted-bounds lookbacks, max-span
// clamping, hard MaxBuckets (5000) cardinality cap keeping newest buckets in
// ascending order, inverted windows, invalid durations, and cancellation.
func TestTorrentMetricsBoundedCardinalityPostgres(t *testing.T) {
	pool, gdb := newTorrentMetricsTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	// Seed 6500 distinct minute buckets over the last ~4.5 days across 2 sources
	// plus 200 historical buckets from 2 years ago.
	_, err := pool.Exec(ctx, `
INSERT INTO torrents_torrent_sources (source, info_hash, created_at, updated_at)
SELECT
	CASE WHEN g % 2 = 0 THEN 'src_a' ELSE 'src_b' END,
	decode(lpad(to_hex(g), 40, '0'), 'hex'),
	$1::timestamptz - (g || ' minutes')::interval,
	$1::timestamptz - (g || ' minutes')::interval
FROM generate_series(0, 6499) AS g
UNION ALL
SELECT
	'src_old',
	decode(lpad(to_hex(100000 + g), 40, '0'), 'hex'),
	$1::timestamptz - interval '730 days' - (g || ' minutes')::interval,
	$1::timestamptz - interval '730 days' - (g || ' minutes')::interval
FROM generate_series(0, 199) AS g
`, now)
	require.NoError(t, err)

	c := client{db: gdb, now: func() time.Time { return now }}

	t.Run("omitted bounds apply 24h default minute lookback", func(t *testing.T) {
		buckets, reqErr := c.Request(ctx, Request{BucketDuration: metrics.BucketDurationMinute})
		require.NoError(t, reqErr)
		require.NotEmpty(t, buckets)
		require.LessOrEqual(t, len(buckets), 1442)
		cutoff := now.Add(-metrics.DefaultMinuteWindow).Truncate(time.Minute)
		for _, b := range buckets {
			require.NotEqual(t, "src_old", b.Source)
			require.False(t, b.Bucket.Before(cutoff))
		}
	})

	t.Run("wide explicit range caps strictly at MaxBuckets keeping newest ordered ascending", func(t *testing.T) {
		buckets, reqErr := c.Request(ctx, Request{
			BucketDuration: metrics.BucketDurationMinute,
			StartTime:      now.Add(-90 * 24 * time.Hour),
			EndTime:        now,
		})
		require.NoError(t, reqErr)
		require.Len(t, buckets, metrics.MaxBuckets)
		// Verify newest bucket (now) is retained and rows are sorted ascending
		// by (source, bucket, updated).
		hasNewest := false
		for i, b := range buckets {
			require.NotEqual(t, "src_old", b.Source)
			if b.Bucket.Equal(now) {
				hasNewest = true
			}
			if i > 0 {
				prev := buckets[i-1]
				require.True(t, prev.Source < b.Source || (prev.Source == b.Source && !prev.Bucket.After(b.Bucket)))
			}
		}
		require.True(t, hasNewest, "capped result must retain the newest bucket")
	})

	t.Run("inverted window returns empty without error", func(t *testing.T) {
		buckets, reqErr := c.Request(ctx, Request{
			BucketDuration: metrics.BucketDurationHour,
			StartTime:      now,
			EndTime:        now.Add(-time.Hour),
		})
		require.NoError(t, reqErr)
		require.Empty(t, buckets)
	})

	t.Run("invalid bucket duration returns error", func(t *testing.T) {
		_, reqErr := c.Request(ctx, Request{BucketDuration: "year"})
		require.Error(t, reqErr)
	})

	t.Run("cancelled context aborts promptly", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		_, reqErr := c.Request(cancelCtx, Request{BucketDuration: metrics.BucketDurationMinute})
		require.ErrorIs(t, reqErr, context.Canceled)
	})
}
