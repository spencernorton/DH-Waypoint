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

func TestParadeDBBM25SupportMigrationUpDownUp(t *testing.T) {
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
	_, err = admin.ExecContext(ctx, `SELECT pg_advisory_lock(830572);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830572)`)
	require.NoError(t, err)
	schema := fmt.Sprintf("dh_waypoint_bm25_mig_test_%d", time.Now().UnixNano())
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

	// Stock installs get zero storage bloat: the migration adds functions
	// only, never generated or plain search columns.
	var columnCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM pg_attribute
WHERE attrelid = to_regclass('torrent_contents') AND attname IN ('search_text', 'ep_text') AND NOT attisdropped`).Scan(&columnCount))
	require.Equal(t, 0, columnCount)

	var text string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT torrent_contents_tsv_text('''1080p'':2A ''shrek'':1A'::tsvector)`).Scan(&text))
	require.Equal(t, "1080p shrek", text)
	var nullText sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT torrent_contents_tsv_text(NULL)`).Scan(&nullText))
	require.False(t, nullText.Valid)

	for input, want := range map[string]string{
		`{"2":{"5":{},"6":{}},"3":{}}`: "s2e5 s2e6 s3all",
		`{}`:                           "",
		`[]`:                           "",
		`{"2":[1]}`:                    "",
	} {
		var got string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT torrent_contents_ep_tokens($1::jsonb)`, input).Scan(&got), input)
		require.Equal(t, want, got, input)
	}
	require.NoError(t, db.QueryRowContext(ctx, `SELECT torrent_contents_ep_tokens(NULL)`).Scan(&text))
	require.Equal(t, "", text)

	var prosrc string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE pronamespace = current_schema()::regnamespace AND proname = 'processor_publication_source_changed'`).Scan(&prosrc))
	require.Contains(t, prosrc, "dh_waypoint.suppress_publication_trigger")
	require.Contains(t, prosrc, "'search_text'")
	require.Contains(t, prosrc, "qb_state_observation", "the 00078 delete fence must be preserved")

	// Trigger behavior on one dedicated connection so the suppression GUC
	// stays on the session under test.
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	h := make([]byte, 20)
	h[0] = 0x43
	_, err = conn.ExecContext(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,'Synthetic BM25 Migration',1024,false,'single',now(),now())`, h)
	require.NoError(t, err)
	insertEvidence := func(id string) {
		t.Helper()
		_, execErr := conn.ExecContext(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength)
VALUES ('synthetic','synthetic_kind','inst',$2,$1,'tv',now(),5)`, h, id)
		require.NoError(t, execErr)
	}
	revision := func() int64 {
		t.Helper()
		var rev int64
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&rev))
		return rev
	}
	insertEvidence("bm25-obs-1")
	rev1 := revision()

	_, err = conn.ExecContext(ctx, `SELECT set_config('dh_waypoint.suppress_publication_trigger','on',false)`)
	require.NoError(t, err)
	insertEvidence("bm25-obs-2")
	require.Equal(t, rev1, revision(), "suppressed insert must not bump the source revision")
	_, err = conn.ExecContext(ctx, `DELETE FROM label_evidence WHERE source_object_id='bm25-obs-2'`)
	require.NoError(t, err, "suppressed delete must not error")
	require.Equal(t, rev1, revision(), "suppressed delete must not bump the source revision")
	var remaining int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM label_evidence WHERE source_object_id='bm25-obs-2'`).Scan(&remaining))
	require.Equal(t, 0, remaining)

	_, err = conn.ExecContext(ctx, `SELECT set_config('dh_waypoint.suppress_publication_trigger','off',false)`)
	require.NoError(t, err)
	insertEvidence("bm25-obs-3")
	require.Greater(t, revision(), rev1, "unsuppressed insert must bump the source revision")
	rev2 := revision()

	// Simulate the opt-in columns as plain columns: search_text/ep_text-only
	// updates must be ignored by the trigger comparison.
	h2 := make([]byte, 20)
	h2[0] = 0x44
	_, err = conn.ExecContext(ctx, `ALTER TABLE torrent_contents ADD COLUMN search_text text, ADD COLUMN ep_text text`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,'Synthetic BM25 Contents',1024,false,'single',now(),now())`, h2)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO torrent_contents(info_hash,created_at,updated_at) VALUES($1,now(),now())`, h2)
	require.NoError(t, err)
	var contentsRev int64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h2).Scan(&contentsRev))
	_, err = conn.ExecContext(ctx, `UPDATE torrent_contents SET search_text='synthetic', ep_text='s1all' WHERE info_hash=$1`, h2)
	require.NoError(t, err)
	var afterSearchRev int64
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h2).Scan(&afterSearchRev))
	require.Equal(t, contentsRev, afterSearchRev, "search_text/ep_text-only updates must not bump the source revision")
	_, err = conn.ExecContext(ctx, `UPDATE torrent_contents SET size=size+1 WHERE info_hash=$1`, h2)
	require.NoError(t, err)
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h2).Scan(&afterSearchRev))
	require.Greater(t, afterSearchRev, contentsRev, "substantive updates must still bump the source revision")
	require.Equal(t, rev2, revision(), "contents updates for another hash must not affect this revision")

	// Down restores the 00078 trigger body (with its delete fence) and drops
	// only the helper functions.
	_, err = provider.DownTo(ctx, 79)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE pronamespace = current_schema()::regnamespace AND proname = 'processor_publication_source_changed'`).Scan(&prosrc))
	require.NotContains(t, prosrc, "suppress_publication_trigger")
	require.Contains(t, prosrc, "qb_state_observation")
	var fnExists bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regprocedure('torrent_contents_tsv_text(tsvector)') IS NOT NULL OR to_regprocedure('torrent_contents_ep_tokens(jsonb)') IS NOT NULL`).Scan(&fnExists))
	require.False(t, fnExists)

	_, err = provider.UpTo(ctx, 80)
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT torrent_contents_ep_tokens('{"1":{}}'::jsonb)`).Scan(&text))
	require.Equal(t, "s1all", text)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT prosrc FROM pg_proc WHERE pronamespace = current_schema()::regnamespace AND proname = 'processor_publication_source_changed'`).Scan(&prosrc))
	require.Contains(t, prosrc, "dh_waypoint.suppress_publication_trigger")
}
