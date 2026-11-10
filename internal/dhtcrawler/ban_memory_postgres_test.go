package dhtcrawler

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/blocking"
	"github.com/spencernorton/DH-Waypoint/internal/cataloguerecovery"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/banning"
	"github.com/spencernorton/DH-Waypoint/internal/verdicts"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
)

// A torrent the ban check rejects must not be fetched again, in this process
// or after a restart, while catalogue removal stays disabled.
func TestBanCheckRejectionIsRememberedWithoutRemoval(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer admin.Close(ctx)
	schema := fmt.Sprintf("crawl_ban_memory_%d", time.Now().UnixNano())
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
	_, err = pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)
 VALUES ($1,'Synthetic.Imported.Without.Files',2048,false,'no_info',now(),now())`, protocol.InfoHashV1(protocol.ID{7}).Bytes())
	require.NoError(t, err)

	newManager := func() blocking.Manager {
		m, err := blocking.New(blocking.Params{
			Pool:        lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }),
			PgxPoolWait: &sync.WaitGroup{},
		}).Manager.Get()
		require.NoError(t, err)
		return m
	}
	block := newManager()
	checker := banning.New(banning.Params{}).Checker
	peers := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")}
	rejected, short := protocol.ID{7}, protocol.ID{8}
	wanted := []protocol.InfoHash{protocol.InfoHashV1(rejected), protocol.InfoHashV1(short)}

	// Under 1 KiB: the size rule rejects it.
	c := crawler{
		metainfoRequester: &policyRequester{info: metainfo.Info{Name: "Synthetic.Tiny.File.txt", Length: 12}},
		csamBlocklist:     policyCsam{}, banningChecker: checker, blockingManager: block,
		verdicts: verdicts.NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })),
	}
	_, err = c.doRequestMetaInfo(ctx, rejected, peers)
	require.ErrorContains(t, err, "size too small")

	// The ledger labels it as a ban-check rejection, never as a CSAM match.
	var mechanism, source string
	require.NoError(t, pool.QueryRow(ctx, `SELECT mechanism,evidence->>'source' FROM torrent_verdict_events WHERE info_hash=$1`,
		protocol.InfoHashV1(rejected).Bytes()).Scan(&mechanism, &source))
	require.Equal(t, verdicts.MechanismBlocking, mechanism)
	require.Equal(t, "ban_check", source)

	// A five-byte title is a real title, not a rejection.
	c.metainfoRequester = &policyRequester{info: metainfo.Info{Name: "Tenet", Length: 2 << 30}}
	res, err := c.doRequestMetaInfo(ctx, short, peers)
	require.NoError(t, err)
	require.Equal(t, "Tenet", res.Info.BestName())

	kept, err := block.Filter(ctx, wanted)
	require.NoError(t, err)
	require.Equal(t, []protocol.InfoHash{protocol.InfoHashV1(short)}, kept)

	// Removal is still held, and the stored row for the rejected hash is intact.
	require.ErrorIs(t, block.Block(ctx, wanted, "blocking", "synthetic", true), cataloguerecovery.ErrDisabled)
	var stored int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrents`).Scan(&stored))
	require.Equal(t, 1, stored)

	// The memory survives a restart: a new manager reads it back from the database.
	require.NoError(t, block.Flush(ctx))
	kept, err = newManager().Filter(ctx, wanted)
	require.NoError(t, err)
	require.Equal(t, []protocol.InfoHash{protocol.InfoHashV1(short)}, kept)
}
