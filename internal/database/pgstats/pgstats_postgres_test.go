package pgstats

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestCollectorEmitsBM25AndFileBundleMetrics(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830574);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830574)`)
	require.NoError(t, err)
	schema := fmt.Sprintf("pgstats_feat_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
	})
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)

	collectValues := func() map[string]float64 {
		reg := prometheus.NewRegistry()
		col := NewCollector(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), zap.NewNop().Sugar())
		require.NoError(t, reg.Register(col))
		mfs, gErr := reg.Gather()
		require.NoError(t, gErr)
		out := map[string]float64{}
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				if m.GetGauge() != nil && len(m.GetLabel()) == 0 {
					out[mf.GetName()] = m.GetGauge().GetValue()
				}
			}
		}
		return out
	}

	v0 := collectValues()
	require.Equal(t, 0.0, v0["dh_waypoint_postgres_bm25_index_valid"])
	require.Equal(t, 0.0, v0["dh_waypoint_postgres_bm25_index_size_bytes"])
	require.Equal(t, 0.0, v0["dh_waypoint_postgres_file_bundles_backfill_done"])

	_, err = pool.Exec(ctx, `INSERT INTO key_values(key,value,created_at,updated_at) VALUES('storage_file_bundles_backfill','{"last_hash":"abcd","done":true}',now(),now())`)
	require.NoError(t, err)

	v1 := collectValues()
	require.Equal(t, 1.0, v1["dh_waypoint_postgres_file_bundles_backfill_done"])
}
