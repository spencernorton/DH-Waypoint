package dhtcrawler

import (
	"context"
	"crypto/sha1"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// When the crawler fetches the metadata of a hash that is already stored, for
// example one imported with a name and an approximate size, the row takes the
// real size, and it is private if the row or the metadata says so. This runs
// the real persist transaction.
func TestCrawlerUpdatesSizeAndPrivateOfStoredHash(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer admin.Close(ctx)
	schema := fmt.Sprintf("crawl_update_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	defer admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	migration, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = migration.Up(ctx)
	require.NoError(t, err)
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)

	batches := measuredBatches{make(chan []infoHashWithMetaInfo, 1)}
	scrapes := measuredScrapes{make(chan nodeHasPeersForHash, 3)}
	c := crawler{dao: dao.Use(g), saveFilesThreshold: 256, persistTorrents: batches, scrape: scrapes, logger: zap.NewNop().Sugar(),
		persistedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{Name: "dh_waypoint_test_update_persisted_total", Help: "Synthetic test counter"}, []string{"entity"})}
	active, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.runPersistTorrents(active) }()
	defer func() { cancel(); <-done }()

	// Two files, 1,050 bytes in total.
	fetched := func(name string, private bool) infoHashWithMetaInfo {
		fields := map[string]any{"name": name, "piece length": 16384, "pieces": make([]byte, 20), "files": []any{
			map[string]any{"length": 1000, "path": []string{"a.mkv"}}, map[string]any{"length": 50, "path": []string{"b.srt"}}}}
		if private {
			fields["private"] = 1
		}
		raw, err := bencode.Marshal(fields)
		require.NoError(t, err)
		h := protocol.InfoHashV1(protocol.ID(sha1.Sum(raw)))
		validated, err := metainfo.ParseDiscoveredInfo(h.WireID(), raw)
		require.NoError(t, err)
		return infoHashWithMetaInfo{nodeHasPeersForHash: nodeHasPeersForHash{infoHash: h.WireID()}, validated: validated}
	}
	cases := []struct {
		name                         string
		stored, storedPrivate, fetch bool
	}{
		{name: "Synthetic.Stored.Public.Fetched.Private", stored: true, fetch: true},
		{name: "Synthetic.Stored.Private.Fetched.Public", stored: true, storedPrivate: true},
		{name: "Synthetic.Stored.Public.Fetched.Public", stored: true},
		{name: "Synthetic.New.Fetched.Private", fetch: true},
	}
	batch := make([]infoHashWithMetaInfo, len(cases))
	for i, tc := range cases {
		batch[i] = fetched(tc.name, tc.fetch)
		if tc.stored {
			// Stored first with a rounded size and no file list.
			_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,$2,262144,$3,'no_info',now(),now())`,
				batch[i].validated.Identities.Canonical().Bytes(), tc.name, tc.storedPrivate)
			require.NoError(t, err)
		}
	}
	batches.batches <- batch
	for range batch {
		select {
		case <-scrapes.done:
		case <-time.After(10 * time.Second):
			t.Fatal("crawler did not persist the batch")
		}
	}
	for i, tc := range cases {
		var size, files int
		var private bool
		var status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT size,private,files_status,files_count FROM torrents WHERE info_hash=$1`,
			batch[i].validated.Identities.Canonical().Bytes()).Scan(&size, &private, &status, &files))
		require.Equal(t, 1050, size, tc.name)
		require.Equal(t, tc.storedPrivate || tc.fetch, private, tc.name)
		require.Equal(t, "multi", status, tc.name)
		require.Equal(t, 2, files, tc.name)
	}
}
