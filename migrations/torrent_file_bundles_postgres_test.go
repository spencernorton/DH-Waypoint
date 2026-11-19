package migrationssql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

// Migration 00081 applies cleanly on top of 00080, serves bundle-first
// reads with NULL extensions intact, enqueues publication work for bundle
// writes unless suppressed, and rolls back cleanly.
func TestTorrentFileBundlesUpDown(t *testing.T) {
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
	schema := fmt.Sprintf("dh_waypoint_bundles_mig_test_%d", time.Now().UnixNano())
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

	_, err = provider.UpTo(ctx, 80)
	require.NoError(t, err)
	var tableName sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('torrent_file_bundles')::text`).Scan(&tableName))
	require.False(t, tableName.Valid, "bundle table must not exist at 00080")

	_, err = provider.UpTo(ctx, 81)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('torrent_file_bundles')::text`).Scan(&tableName))
	require.True(t, tableName.Valid)

	h1 := make([]byte, 20)
	h1[0] = 0x81
	h2 := make([]byte, 20)
	h2[0] = 0x82
	for _, h := range [][]byte{h1, h2} {
		_, err = db.ExecContext(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,'Synthetic.Bundle',15,false,'multi',2,now(),now())`, h)
		require.NoError(t, err)
	}
	// Legacy rows, one with a generated NULL extension.
	_, err = db.ExecContext(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Bundle/film.mkv',10,now(),now()),($1,1,'README',5,now(),now())`, h1)
	require.NoError(t, err)
	// Bundle rows for h2 shadow its decoy legacy row; NULL extension survives.
	_, err = db.ExecContext(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,9,'shadowed.mkv',1,now(),now())`, h2)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO torrent_file_bundles(info_hash,indexes,paths,sizes,extensions) VALUES($1,ARRAY[0,1],ARRAY['Bundle/film.mkv','README'],ARRAY[10,5]::bigint[],ARRAY['mkv',NULL]::text[])`, h2)
	require.NoError(t, err)

	type fileRow struct {
		path string
		ext  sql.NullString
	}
	readForHash := func(h []byte) []fileRow {
		rows, err := db.QueryContext(ctx, `SELECT path,extension FROM torrent_files_for_hash($1) ORDER BY "index",path`, h)
		require.NoError(t, err)
		defer rows.Close()
		var out []fileRow
		for rows.Next() {
			var r fileRow
			require.NoError(t, rows.Scan(&r.path, &r.ext))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}
	legacy := readForHash(h1)
	bundled := readForHash(h2)
	require.Equal(t, legacy, bundled, "bundle and legacy reads must be identical")
	require.Len(t, legacy, 2)
	require.False(t, legacy[1].ext.Valid, "extension-free path keeps NULL extension")
	var viewCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM torrent_files_effective WHERE path='shadowed.mkv'`).Scan(&viewCount))
	require.Zero(t, viewCount, "bundled hash must shadow legacy rows in the view")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM torrent_files_effective`).Scan(&viewCount))
	require.Equal(t, 4, viewCount)

	// A plain bundle write enqueues publication work; a suppressed write
	// (the backfill fence) must not move the revision or pending row.
	revision := func() (int64, int64, int64) {
		var rev, pendingCount, pendingRev int64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h1).Scan(&rev))
		require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(source_revision),0) FROM processor_publication_pending WHERE info_hash=$1`, h1).Scan(&pendingCount, &pendingRev))
		return rev, pendingCount, pendingRev
	}
	rev0, _, _ := revision()
	_, err = db.ExecContext(ctx, `INSERT INTO torrent_file_bundles(info_hash,indexes,paths,sizes,extensions) VALUES($1,ARRAY[0],ARRAY['x.mkv'],ARRAY[1]::bigint[],ARRAY['mkv'])`, h1)
	require.NoError(t, err)
	rev1, pend1, pendRev1 := revision()
	require.Equal(t, rev0+1, rev1, "bundle insert must enqueue like a legacy file insert")
	require.EqualValues(t, 1, pend1)
	require.Equal(t, rev1, pendRev1)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT set_config('dh_waypoint.suppress_publication_trigger','on',true)`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE torrent_file_bundles SET sizes=ARRAY[2]::bigint[] WHERE info_hash=$1`, h1)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,7,'suppressed.mkv',1,now(),now())`, h1)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM torrent_files WHERE info_hash=$1 AND "index"=7`, h1)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	rev2, pend2, pendRev2 := revision()
	require.Equal(t, rev1, rev2, "suppressed writes must not bump the publication revision")
	require.Equal(t, pend1, pend2)
	require.Equal(t, pendRev1, pendRev2)

	_, err = provider.DownTo(ctx, 80)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('torrent_file_bundles')::text`).Scan(&tableName))
	require.False(t, tableName.Valid, "down must drop the bundle table")
	var prosrc string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE pronamespace = current_schema()::regnamespace AND proname = 'processor_publication_source_changed'`).Scan(&prosrc))
	require.Contains(t, prosrc, "dh_waypoint.suppress_publication_trigger", "00081 down must preserve 00080 trigger function")
	_, err = provider.UpTo(ctx, 81)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('torrent_file_bundles')::text`).Scan(&tableName))
	require.True(t, tableName.Valid, "re-up restores the bundle table")
}
