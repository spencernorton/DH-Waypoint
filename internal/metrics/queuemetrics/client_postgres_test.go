package queuemetrics

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

func newQueueMetricsTestDB(t *testing.T) (*pgxpool.Pool, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("queuemetrics_%d", time.Now().UnixNano())
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
CREATE TABLE queue_jobs (
	id bigserial PRIMARY KEY,
	queue text NOT NULL,
	status text NOT NULL,
	created_at timestamptz NOT NULL,
	run_after timestamptz NOT NULL,
	ran_at timestamptz
);
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

// Synthetic many-bucket regression verifying omitted-bounds defaults, fixed OR
// filter precedence on pending vs non-pending rows, hard MaxBuckets (5000) cap,
// ascending order, and context cancellation.
func TestQueueMetricsBoundedCardinalityPostgres(t *testing.T) {
	pool, gdb := newQueueMetricsTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	// Seed 6200 distinct minute buckets over ~4.3 days + 100 2-year-old completed
	// jobs + 5 recent pending jobs (ran_at IS NULL).
	_, err := pool.Exec(ctx, `
INSERT INTO queue_jobs (queue, status, created_at, run_after, ran_at)
SELECT
	CASE WHEN g % 2 = 0 THEN 'q_alpha' ELSE 'q_beta' END,
	'processed',
	$1::timestamptz - (g || ' minutes')::interval - interval '5 seconds',
	$1::timestamptz - (g || ' minutes')::interval - interval '3 seconds',
	$1::timestamptz - (g || ' minutes')::interval
FROM generate_series(0, 6199) AS g
UNION ALL
SELECT
	'q_ancient',
	'processed',
	$1::timestamptz - interval '730 days' - (g || ' minutes')::interval,
	$1::timestamptz - interval '730 days' - (g || ' minutes')::interval,
	$1::timestamptz - interval '730 days' - (g || ' minutes')::interval
FROM generate_series(0, 99) AS g
UNION ALL
SELECT
	'q_pending',
	'pending',
	$1::timestamptz - (g || ' minutes')::interval,
	$1::timestamptz - (g || ' minutes')::interval,
	NULL
FROM generate_series(0, 4) AS g
`, now)
	require.NoError(t, err)

	c := client{db: gdb, now: func() time.Time { return now }}

	t.Run("omitted bounds apply 24h default and exclude ancient completed jobs", func(t *testing.T) {
		buckets, reqErr := c.Request(ctx, Request{BucketDuration: metrics.BucketDurationMinute})
		require.NoError(t, reqErr)
		require.NotEmpty(t, buckets)
		require.LessOrEqual(t, len(buckets), 1450)
		hasPending := false
		for _, b := range buckets {
			require.NotEqual(t, "q_ancient", b.Queue, "ancient completed jobs must be filtered out by StartTime")
			if b.Queue == "q_pending" {
				hasPending = true
				require.Nil(t, b.Latency)
			}
		}
		require.True(t, hasPending)
	})

	t.Run("wide explicit range caps strictly at MaxBuckets keeping newest ordered ascending", func(t *testing.T) {
		buckets, reqErr := c.Request(ctx, Request{
			BucketDuration: metrics.BucketDurationMinute,
			StartTime:      now.Add(-90 * 24 * time.Hour),
			EndTime:        now,
		})
		require.NoError(t, reqErr)
		require.Len(t, buckets, metrics.MaxBuckets)
		hasNewest := false
		for i, b := range buckets {
			require.NotEqual(t, "q_ancient", b.Queue)
			if b.RanAtBucket.Equal(now) {
				hasNewest = true
				require.NotNil(t, b.Latency)
			}
			if i > 0 {
				prev := buckets[i-1]
				require.True(t, prev.Queue < b.Queue || (prev.Queue == b.Queue && prev.Status <= b.Status))
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
		_, reqErr := c.Request(ctx, Request{BucketDuration: "week"})
		require.Error(t, reqErr)
	})

	t.Run("cancelled context aborts promptly", func(t *testing.T) {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		_, reqErr := c.Request(cancelCtx, Request{BucketDuration: metrics.BucketDurationMinute})
		require.ErrorIs(t, reqErr, context.Canceled)
	})
}
