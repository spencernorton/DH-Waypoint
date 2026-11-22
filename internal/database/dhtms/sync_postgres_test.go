package dhtms

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestSyncerPostgresAndLiveDaemonE2E proves the complete Stage 2 Phase 2 sync contract
// against real PostgreSQL + live dh-tmsd:
//   - empty-volume auto-bootstrap via FullBuild (zero manual setup)
//   - exact serving.Policy visibility parity (public vs private/adult/hold/exclusion/quarantine)
//   - 25-file bundle deep token indexing (file #20)
//   - 00101_dhtms_outbox DB triggers firing on raw SQL writes (S2-7)
//   - immediate fail-closed revocation, live L0 overlay, live swarm update, new-hash rebuild
//   - fail-closed lease expiry (503 ErrLeaseExpired) and monotonic LSN recovery
func TestSyncerPostgresAndLiveDaemonE2E(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("DH_WAYPOINT_TEST_POSTGRES_DSN not set; skipping PG + dh-tmsd E2E test")
	}
	cli, _ := startTestDaemon(t)
	ctx := context.Background()

	// Create an isolated disposable database so search_path & triggers match production.
	adminCfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	adminDB := stdlib.OpenDB(*adminCfg)
	t.Cleanup(func() { _ = adminDB.Close() })

	dbName := fmt.Sprintf("dhw_dhtms_sync_%d", time.Now().UnixNano())
	quotedDB := pgx.Identifier{dbName}.Sanitize()
	_, err = adminDB.ExecContext(ctx, "CREATE DATABASE "+quotedDB)
	require.NoError(t, err)

	testCfg := adminCfg.Copy()
	testCfg.Database = dbName
	sqlDB := stdlib.OpenDB(*testCfg)
	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	poolCfg.ConnConfig.Database = dbName
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_ = sqlDB.Close()
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = adminDB.ExecContext(cleanCtx, "DROP DATABASE IF EXISTS "+quotedDB+" WITH (FORCE)")
	})

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)

	// Seed catalog + content_attributes (IMDB alias) + torrents spanning every visibility edge case.
	_, err = pool.Exec(ctx, `
INSERT INTO content (type, source, id, title, original_title, release_year, adult, created_at, updated_at)
VALUES ('movie', 'tmdb', '629', 'The Usual Suspects', 'The Usual Suspects', 1995, false, now(), now());
INSERT INTO content_attributes (content_type, content_source, content_id, source, key, value, created_at, updated_at)
VALUES ('movie', 'tmdb', '629', 'imdb', 'id', 'tt0114814', now(), now());`)
	require.NoError(t, err)

	hashPub := strings.Repeat("a1", 20)
	hashUnlinked := strings.Repeat("a2", 20)
	hashPrivate := strings.Repeat("b1", 20)
	hashAdult := strings.Repeat("b2", 20)
	hashHeld := strings.Repeat("b3", 20)
	hashExcluded := strings.Repeat("b4", 20)
	hashNew := strings.Repeat("c1", 20)

	insertTorrent := func(hHex, name string, size int64, priv bool, filesStatus string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
INSERT INTO torrents (info_hash, name, size, private, files_status, created_at, updated_at)
VALUES (decode($1, 'hex'), $2, $3, $4, $5, now(), now())`, hHex, name, size, priv, filesStatus)
		require.NoError(t, err)
	}

	insertTorrent(hashPub, "The.Usual.Suspects.1995.1080p.BluRay.x264", 8_000_000_000, false, "multi")
	insertTorrent(hashUnlinked, "The.Usual.Suspects.1995.720p.WEBRip", 3_000_000_000, false, "single")
	insertTorrent(hashPrivate, "Private.Suspects.1995.1080p", 5_000_000_000, true, "single")
	insertTorrent(hashAdult, "FC2-PPV-9876543.1080p.mp4", 2_000_000_000, false, "single")
	insertTorrent(hashHeld, "Held.Suspects.1995.1080p", 4_000_000_000, false, "single")
	insertTorrent(hashExcluded, "Excluded.Suspects.1995.1080p", 4_000_000_000, false, "single")

	_, err = pool.Exec(ctx, `
INSERT INTO torrent_contents (info_hash, content_type, content_source, content_id, video_resolution, seeders, leechers, published_at, size, created_at, updated_at)
VALUES (decode($1, 'hex'), 'movie', 'tmdb', '629', 'V1080p', 55, 7, now(), 8000000000, now(), now())`, hashPub)
	require.NoError(t, err)

	// Attach a 25-file bundle to hashPub with a distinct token in file #20.
	idxArr := make([]int32, 25)
	pathsArr := make([]string, 25)
	sizesArr := make([]int64, 25)
	extsArr := make([]string, 25)
	for i := range 25 {
		idxArr[i] = int32(i)
		pathsArr[i] = fmt.Sprintf("Disc1/Track_%02d.mkv", i+1)
		if i == 19 {
			pathsArr[i] = "Disc1/Bonus/UniqueSyncDeepToken20.mkv"
		}
		sizesArr[i] = 300_000_000
		extsArr[i] = "mkv"
	}
	_, err = pool.Exec(ctx, `
INSERT INTO torrent_file_bundles (info_hash, indexes, paths, sizes, extensions)
VALUES (decode($1, 'hex'), $2, $3, $4, $5)`, hashPub, idxArr, pathsArr, sizesArr, extsArr)
	require.NoError(t, err)

	// Add hold + exclusion rows via raw SQL.
	_, err = pool.Exec(ctx, `
INSERT INTO processor_publication_holds (info_hash, held, reason, request_id)
VALUES (decode($1, 'hex'), true, 'operator_review', repeat('1', 64))`, hashHeld)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO processor_exclusions (info_hash, job_id, processor_id, binding, reason)
VALUES (decode($1, 'hex'), 'job-1', 'core', '{}'::jsonb, 'policy_denied')`, hashExcluded)
	require.NoError(t, err)

	// Verify 00101 triggers populated dhtms_outbox automatically!
	var outboxBefore int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM dhtms_outbox`).Scan(&outboxBefore))
	require.GreaterOrEqual(t, outboxBefore, int64(6), "00101 triggers must capture raw SQL writes")

	strongKW, mediaExt, err := classifier.CoreAdultServingEvidence()
	require.NoError(t, err)
	pol, err := serving.NewPolicy(serving.Config{
		ProcessorExclusions: true,
		ExcludeAdult:        true,
	}, strongKW, mediaExt)
	require.NoError(t, err)

	syncCfg := DefaultConfig()
	syncCfg.Mode = ReadModeServe
	syncCfg.LeaseTTL = 60 * time.Second
	syncCfg.RebuildDebounce = 0
	metrics := NewMetrics()
	syncer := NewSyncer(pool, cli, pol, syncCfg, metrics, zap.NewNop().Sugar())

	// 1. First SyncOnce auto-bootstraps empty dh-tmsd (gen 0 -> gen 1) and drains outbox.
	require.NoError(t, syncer.SyncOnce(ctx))

	var outboxAfterBuild int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM dhtms_outbox`).Scan(&outboxAfterBuild))
	require.Equal(t, int64(0), outboxAfterBuild)

	health, err := cli.Health(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, health.GenerationID, uint64(1))
	require.True(t, health.LeaseActive)
	require.Equal(t, 6, health.NumDocs)

	// 2. Compare PG TorrentCondition("torrents") eligible set against dh-tmsd strictly (0 leaks!).
	tcExpr := pol.TorrentCondition("torrents")
	pgCond, pgArgs, _ := GormExprToPG(tcExpr.SQL, tcExpr.Vars, 1)
	pgRows, err := pool.Query(ctx, "SELECT info_hash FROM torrents WHERE "+pgCond, pgArgs...)
	require.NoError(t, err)
	var pgEligible []string
	for pgRows.Next() {
		var h []byte
		require.NoError(t, pgRows.Scan(&h))
		pgEligible = append(pgEligible, hex.EncodeToString(h))
	}
	pgRows.Close()
	require.ElementsMatch(t, []string{hashPub, hashUnlinked}, pgEligible)

	searchRes, err := cli.Search(ctx, SearchRequest{Query: "suspects", Mode: ModeAnd, Tier: TierBase, Limit: 20})
	require.NoError(t, err)
	comp := NewShadowComparator()
	diff := comp.CompareStrict(pgEligible, searchRes.Docs, time.Millisecond, time.Millisecond)
	require.True(t, diff.ExactSet, "expected exact zero-leak parity: %+v", diff)
	require.Empty(t, diff.LeakedHashes)

	// Verify file #20 token search works from torrent_file_bundles!
	deepRes, err := cli.Search(ctx, SearchRequest{Query: "uniquesyncdeeptoken20", Mode: ModeAnd, Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.Len(t, deepRes.Docs, 1)
	require.Equal(t, hashPub, deepRes.Docs[0].InfoHashV1)

	// Verify IMDB lookup returns both linked + unlinked fallback!
	lu, err := cli.Lookup(ctx, LookupRequest{
		ContentType: 1, ContentSource: "imdb", ContentID: "tt0114814", Tier: TierBase, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, lu.Linked, 1)
	require.Len(t, lu.Unlinked, 1)

	// 3. Live S2-7 trigger CDC: raw SQL hold on hashUnlinked -> SyncOnce immediately revokes (0 leaks).
	_, err = pool.Exec(ctx, `
INSERT INTO processor_publication_holds (info_hash, held, reason, request_id)
VALUES (decode($1, 'hex'), true, 'live_revoke', repeat('2', 64))`, hashUnlinked)
	require.NoError(t, err)
	require.NoError(t, syncer.SyncOnce(ctx))

	hlRevoked, err := cli.HashLookup(ctx, HashLookupRequest{InfoHash: hashUnlinked, Tier: TierBase})
	require.NoError(t, err)
	require.True(t, hlRevoked.Found)
	require.False(t, hlRevoked.Eligible, "live hold trigger must fail-closed revoke immediately")

	// 4. Live swarm update + L0 metadata overlay via raw SQL triggers (without full rebuild).
	genBeforeSwarm := health.GenerationID
	_, err = pool.Exec(ctx, `
UPDATE torrent_contents SET seeders = 777, leechers = 12, updated_at = now()
WHERE info_hash = decode($1, 'hex')`, hashPub)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
UPDATE torrents SET name = 'The.Usual.Suspects.Remastered.Overlay.1995.1080p', updated_at = now()
WHERE info_hash = decode($1, 'hex')`, hashPub)
	require.NoError(t, err)
	require.NoError(t, syncer.SyncOnce(ctx))

	hAfterL0, err := cli.Health(ctx)
	require.NoError(t, err)
	require.Equal(t, genBeforeSwarm, hAfterL0.GenerationID, "L0 overlay + swarm update must not require rebuild")

	ovRes, err := cli.Search(ctx, SearchRequest{Query: "remastered overlay", Mode: ModeAnd, Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.Len(t, ovRes.Docs, 1)
	require.Equal(t, hashPub, ovRes.Docs[0].InfoHashV1)
	require.NotNil(t, ovRes.Docs[0].Seeders)
	require.Equal(t, uint32(777), *ovRes.Docs[0].Seeders)

	// 5. Raw SQL insert of brand-new torrent -> SyncOnce rebuilds and serves it automatically!
	insertTorrent(hashNew, "Brand.New.Discovery.2026.2160p.WEB-DL", 12_000_000_000, false, "single")
	require.NoError(t, syncer.SyncOnce(ctx))

	newRes, err := cli.Search(ctx, SearchRequest{Query: "brand new discovery", Mode: ModeAnd, Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.Len(t, newRes.Docs, 1)
	require.Equal(t, hashNew, newRes.Docs[0].InfoHashV1)

	// 6. Fail-closed lease expiry -> 503 ErrLeaseExpired -> SyncOnce recovers with monotonic LSN!
	hNow, err := cli.Health(ctx)
	require.NoError(t, err)
	_, err = cli.PolicyLease(ctx, PolicyLeaseRequest{PGLSN: hNow.PGLSN + 1, TTLMS: 0})
	require.NoError(t, err)
	_, err = cli.Search(ctx, SearchRequest{Query: "brand new discovery", Mode: ModeAnd, Tier: TierBase, Limit: 10})
	require.True(t, errors.Is(err, ErrLeaseExpired), "expected ErrLeaseExpired, got %v", err)

	require.NoError(t, syncer.SyncOnce(ctx))
	recRes, err := cli.Search(ctx, SearchRequest{Query: "brand new discovery", Mode: ModeAnd, Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.Len(t, recRes.Docs, 1)
}
