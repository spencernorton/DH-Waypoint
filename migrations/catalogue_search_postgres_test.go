package migrationssql

import (
	"context"
	"fmt"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
)

func TestCatalogueSearchIndexesUpDown(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	schema := fmt.Sprintf("dh_waypoint_catalogue_index_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, cleanupErr := admin.ExecContext(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, cleanupErr)
	})
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, `
CREATE TABLE torrent_contents (id text PRIMARY KEY, info_hash bytea NOT NULL,
 content_type text, content_source text, content_id text, published_at timestamptz NOT NULL);
CREATE INDEX existing_published_idx ON torrent_contents(published_at);
CREATE TABLE torrents (info_hash bytea PRIMARY KEY, name text NOT NULL,
 tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple',name)) STORED);
CREATE INDEX torrents_tsv_idx ON torrents(tsv);
CREATE TABLE content_attributes (source text, value text, content_type text,
 content_source text, content_id text, key text);
INSERT INTO torrent_contents VALUES ('synthetic',decode(repeat('ab',32),'hex'),'movie','synthetic','123',now());
INSERT INTO torrents(info_hash,name) VALUES (decode(repeat('ab',32),'hex'),'Synthetic title');
INSERT INTO content_attributes VALUES ('imdb','tt123','movie','synthetic','123','id');`)
	require.NoError(t, err)
	const filename = "00072_catalogue_search_indexes.sql"
	migration, err := FS.ReadFile(filename)
	require.NoError(t, err)
	provider, err := goose.NewProvider(goose.DialectPostgres, db,
		fstest.MapFS{filename: &fstest.MapFile{Data: migration}})
	require.NoError(t, err)
	for cycle := 0; cycle < 2; cycle++ {
		_, err = provider.Up(ctx)
		require.NoError(t, err)
		var valid int
		err = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_index i
 JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND i.indisvalid AND i.indisready AND
 c.relname IN ('torrent_contents_identity_published_page_idx',
 'content_attributes_identifier_lookup_idx')`).Scan(&valid)
		require.NoError(t, err)
		require.Equal(t, 2, valid)
		var removed bool
		err = db.QueryRowContext(ctx, `SELECT to_regclass('torrents_tsv_idx') IS NULL`).Scan(&removed)
		require.NoError(t, err)
		require.True(t, removed)
		_, err = provider.Down(ctx)
		require.NoError(t, err)
		var remaining, torrents, attributes int
		err = db.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema()),
 (SELECT count(*) FROM torrent_contents),(SELECT count(*) FROM content_attributes)`).Scan(&remaining, &torrents, &attributes)
		require.NoError(t, err)
		require.Equal(t, 5, remaining) // content/raw PKs, original published/TSV indexes, Goose PK
		require.Equal(t, 1, torrents)
		require.Equal(t, 1, attributes)
		var restored bool
		err = db.QueryRowContext(ctx, `SELECT i.indisvalid AND i.indisready AND am.amname='btree'
 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
 WHERE c.oid=to_regclass('torrents_tsv_idx')`).Scan(&restored)
		require.NoError(t, err)
		require.True(t, restored)
	}
}
