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

func TestV511GovernanceAndConcurrentIndexesUpDown(t *testing.T) {
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
	schema := fmt.Sprintf("dh_waypoint_v511_mig_test_%d", time.Now().UnixNano())
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

	_, err = provider.UpTo(ctx, 79)
	require.NoError(t, err)

	var validNew int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_index i
 JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND i.indisvalid AND i.indisready
 AND c.relname IN ('queue_jobs_pick_idx','torrent_contents_episode_keys_idx','processor_exclusion_events_hash_idx')`).Scan(&validNew)
	require.NoError(t, err)
	require.Equal(t, 3, validNew)

	var droppedCount int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_index i
 JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema()
 AND c.relname IN ('torrents_torrent_sources_seeders_idx','torrents_torrent_sources_leechers_idx','torrents_torrent_sources_created_at_idx','torrents_torrent_sources_updated_at_idx')`).Scan(&droppedCount)
	require.NoError(t, err)
	require.Equal(t, 0, droppedCount)

	// Verify episode key extraction for season packs and multi-episode rows.
	var keys []string
	rows, err := db.QueryContext(ctx, `SELECT unnest(torrent_contents_episode_keys('tv_show','tmdb','1396','{"2":{"5":{},"6":{}},"3":{}}'::jsonb)) ORDER BY 1`)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	require.NoError(t, rows.Close())
	require.Equal(t, []string{
		"tv_show:tmdb:1396:e:2:5",
		"tv_show:tmdb:1396:e:2:6",
		"tv_show:tmdb:1396:s:2",
		"tv_show:tmdb:1396:s:3",
		"tv_show:tmdb:1396:s_all:3",
	}, keys)

	// Verify redundant qb_state_observation delete does not bump source revision,
	// while deleting the last private observation does bump source revision.
	h := make([]byte, 20)
	h[0] = 0x42
	_, err = db.ExecContext(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,'Synthetic Show S01',1024,false,'single',now(),now())`, h)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength)
 VALUES ('qbittorrent','qb_state_observation','qb1','obs-1',$1,'tv',now()-interval '2 hours',5),
        ('qbittorrent','qb_state_observation','qb1','obs-2',$1,'tv',now()-interval '1 hour',5)`, h)
	require.NoError(t, err)
	var revBefore, revAfter int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&revBefore))
	_, err = db.ExecContext(ctx, `DELETE FROM label_evidence WHERE source_object_id='obs-1'`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&revAfter))
	require.Equal(t, revBefore, revAfter, "pruning superseded qb_state_observation must not churn source_revision")

	_, err = db.ExecContext(ctx, `DELETE FROM label_evidence WHERE source_object_id='obs-2'`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&revAfter))
	require.Greater(t, revAfter, revBefore, "deleting the final qb_state_observation must bump source_revision")

	// Step down both 00079 and 00078 and verify restoration.
	_, err = provider.DownTo(ctx, 77)
	require.NoError(t, err)
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_index i
 JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND i.indisvalid
 AND c.relname IN ('torrents_torrent_sources_seeders_idx','torrents_torrent_sources_leechers_idx','torrents_torrent_sources_created_at_idx','torrents_torrent_sources_updated_at_idx')`).Scan(&droppedCount)
	require.NoError(t, err)
	require.Equal(t, 4, droppedCount)
}
