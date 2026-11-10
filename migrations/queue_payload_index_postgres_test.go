package migrationssql

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// Runs the whole chain on an empty schema, then checks that 00075 removes the
// unused queue payload index, keeps the indexes the queue does read, restores
// it on Down, and still applies when the index is already gone.
func TestQueuePayloadIndexDropUpDown(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	_, err = admin.ExecContext(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	schema := fmt.Sprintf("dh_waypoint_queue_index_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, cleanupErr := admin.ExecContext(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	cfg.RuntimeParams["search_path"] = schema + ",public"
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS)
	require.NoError(t, err)

	const index = "queue_jobs_queue_payload_idx"
	indexes := func() []string {
		rows, err := db.QueryContext(ctx, `SELECT c.relname FROM pg_index i
 JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND i.indrelid='queue_jobs'::regclass
 AND i.indisvalid AND i.indisready ORDER BY 1`)
		require.NoError(t, err)
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, rows.Err())
		return names
	}

	_, err = provider.UpTo(ctx, 74)
	require.NoError(t, err)
	before := indexes()
	require.Contains(t, before, index)
	_, err = db.ExecContext(ctx, `INSERT INTO queue_jobs(fingerprint,queue,payload,run_after,archival_duration,created_at)
 VALUES ('synthetic','process_torrent','{"InfoHashes":["ab"]}',now(),'1 hour',now())`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)
	after := indexes()
	require.NotContains(t, after, index)
	require.Len(t, after, len(before)-1) // every other queue index survives

	_, err = provider.Down(ctx)
	require.NoError(t, err)
	require.Equal(t, before, indexes())

	// A database where the index was already removed by hand still migrates.
	_, err = db.ExecContext(ctx, "DROP INDEX "+index)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	require.Equal(t, after, indexes())
	var jobs int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM queue_jobs`).Scan(&jobs))
	require.Equal(t, 1, jobs)
}
