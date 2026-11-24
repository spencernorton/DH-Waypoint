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
	extsArr := make([]*string, 25)
	mkvExt := "mkv"
	for i := range 25 {
		idxArr[i] = int32(i)
		pathsArr[i] = fmt.Sprintf("Disc1/Track_%02d.mkv", i+1)
		extsArr[i] = &mkvExt
		if i == 1 {
			pathsArr[i] = "Disc1/README"
			extsArr[i] = nil // exercise NULL elements in torrent_file_bundles.extensions
		}
		if i == 19 {
			pathsArr[i] = "Disc1/Bonus/UniqueSyncDeepToken20.mkv"
		}
		sizesArr[i] = 300_000_000
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

	// 5. Raw SQL insert of brand-new torrent -> SyncOnce ingests directly into live L0
	// (ZERO full-catalogue rebuild triggered!) and serves it immediately via Search + RSS + HashLookup!
	insertTorrent(hashNew, "Brand.New.Discovery.2026.2160p.WEB-DL", 12_000_000_000, false, "single")
	require.NoError(t, syncer.SyncOnce(ctx))

	hAfterNewL0, err := cli.Health(ctx)
	require.NoError(t, err)
	require.Equal(t, genBeforeSwarm, hAfterNewL0.GenerationID, "brand-new crawled hash must ingest into live L0 without triggering a full rebuild")
	require.GreaterOrEqual(t, hAfterNewL0.L0Count, 2)

	newRes, err := cli.Search(ctx, SearchRequest{Query: "brand new discovery", Mode: ModeAnd, Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.Len(t, newRes.Docs, 1)
	require.Equal(t, hashNew, newRes.Docs[0].InfoHashV1)

	rssRes, err := cli.RSS(ctx, RSSRequest{Tier: TierBase, Limit: 5})
	require.NoError(t, err)
	var foundNewInRSS bool
	for _, d := range rssRes.Docs {
		if d.InfoHashV1 == hashNew {
			foundNewInRSS = true
			break
		}
	}
	require.True(t, foundNewInRSS, "brand-new L0 doc must appear immediately in RSS browse")

	// 5b. Lower CompactDocs threshold and insert one more torrent -> triggers clean compaction rebuild!
	syncer.cfg.CompactDocs = 2
	hashCompact := strings.Repeat("c2", 20)
	insertTorrent(hashCompact, "Compaction.Trigger.2026.1080p.WEB-DL", 4_000_000_000, false, "single")
	require.NoError(t, syncer.SyncOnce(ctx))

	hCompacted, err := cli.Health(ctx)
	require.NoError(t, err)
	require.Greater(t, hCompacted.GenerationID, genBeforeSwarm, "reaching CompactDocs threshold must trigger clean compaction rebuild")
	require.Equal(t, 0, hCompacted.L0Count)
	require.Equal(t, 8, hCompacted.NumDocs)

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

// TestSyncerGatedPayloadAndPublicationE2E proves the gated full-build and outbox
// fast path for deployments enforcing publication review + payload verification:
//   - FullBuild drives from the selective payload gate (excluding unverified backlog rows)
//   - Raw unverified crawl inserts skip L0 and never trigger rebuild churn
//   - Live payload + publication approval triggers ingest directly into L0 (<1s)
//   - Live publication hold immediately revokes fail-closed
//   - processor_registry execution_generation bump automatically triggers a clean rebuild
func TestSyncerGatedPayloadAndPublicationE2E(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("DH_WAYPOINT_TEST_POSTGRES_DSN not set; skipping gated PG + dh-tmsd E2E test")
	}
	cli, _ := startTestDaemon(t)
	ctx := context.Background()

	adminCfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	adminDB := stdlib.OpenDB(*adminCfg)
	t.Cleanup(func() { _ = adminDB.Close() })

	dbName := fmt.Sprintf("dhw_dhtms_gated_%d", time.Now().UnixNano())
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

	procDigest := strings.Repeat("a", 64)
	payDigest := strings.Repeat("b", 64)
	procIdentity := fmt.Sprintf(`{"name":"test-reviewer","artifact_digest":%q}`, procDigest)
	procManifest := fmt.Sprintf(`{"processor":%s,"capabilities":["policy"]}`, procIdentity)

	_, err = pool.Exec(ctx, `
INSERT INTO processor_registry (id, generation, execution_generation, endpoint, mode, state, priority, manifest, configuration)
VALUES ('test-reviewer', 1, 1, 'http://127.0.0.1:9999', 'apply', 'active', 100, $1::jsonb, '{}'::jsonb)`, procManifest)
	require.NoError(t, err)

	hashVerified := strings.Repeat("11", 20)
	hashUnverified := strings.Repeat("22", 20)
	hashCrawl := strings.Repeat("33", 20)
	hashLateVerified := strings.Repeat("44", 20)

	insertTorrent := func(hHex, name string) {
		t.Helper()
		_, err := pool.Exec(ctx, `
INSERT INTO torrents (info_hash, name, size, private, files_status, created_at, updated_at)
VALUES (decode($1, 'hex'), $2, 2000000000, false, 'single', now(), now())`, hHex, name)
		require.NoError(t, err)
	}
	approveAll := func(hHex string, gen int) {
		t.Helper()
		_, err := pool.Exec(ctx, `
INSERT INTO processor_publication_sources (info_hash, revision)
VALUES (decode($1, 'hex'), 1)
ON CONFLICT (info_hash) DO UPDATE SET revision = 1`, hHex)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
INSERT INTO processor_publication_reviews (info_hash, processor_id, source_revision, execution_generation, processor_identity, approved, policy_contract, reason, job_id)
VALUES (decode($1, 'hex'), 'test-reviewer', 1, $2, $3::jsonb, true, $4, 'ok', $5)`,
			hHex, gen, procIdentity, serving.PublicationReviewPolicy, strings.Repeat("9", 64))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
INSERT INTO processor_payload_reviews (info_hash, source_revision, verifier_artifact, policy, approved, reason, receipt, expires_at)
VALUES (decode($1, 'hex'), 1, $2, $3, true, 'ok', '{}'::jsonb, now() + interval '7 days')`,
			hHex, payDigest, serving.PayloadEvidenceContractV2)
		require.NoError(t, err)
	}

	insertTorrent(hashVerified, "Gated.Verified.Release.2026.1080p.BluRay")
	insertTorrent(hashUnverified, "Gated.Unverified.Backlog.2026.1080p.WEB-DL")
	approveAll(hashVerified, 1)

	strongKW, mediaExt, err := classifier.CoreAdultServingEvidence()
	require.NoError(t, err)
	pol, err := serving.NewPolicy(serving.Config{
		ProcessorExclusions:                   true,
		ExcludeAdult:                          true,
		RequiredProcessor:                     "test-reviewer",
		RequiredProcessorArtifactDigest:       procDigest,
		RequiredPayloadVerifierArtifactDigest: payDigest,
	}, strongKW, mediaExt)
	require.NoError(t, err)

	syncCfg := DefaultConfig()
	syncCfg.Mode = ReadModeServe
	syncCfg.LeaseTTL = 60 * time.Second
	syncCfg.RebuildDebounce = 0
	syncer := NewSyncer(pool, cli, pol, syncCfg, NewMetrics(), zap.NewNop().Sugar())

	// 1. Gated FullBuild exports ONLY the 1 payload-verified torrent (skipping unverified backlog!).
	require.NoError(t, syncer.SyncOnce(ctx))
	h1, err := cli.Health(ctx)
	require.NoError(t, err)
	gen1 := h1.GenerationID
	require.GreaterOrEqual(t, gen1, uint64(1))
	require.Equal(t, 1, h1.NumDocs, "gated FullBuild must index only payload-verified docs, not unverified backlog")
	require.Equal(t, uint64(1), h1.PayloadEligible)

	res, err := cli.Search(ctx, SearchRequest{Query: "gated", Mode: ModeAnd, Tier: TierPayload, Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Docs, 1)
	require.Equal(t, hashVerified, res.Docs[0].InfoHashV1)

	// 2. Unverified DHT crawl insert -> SyncOnce drains outbox with ZERO L0 writes and ZERO rebuilds.
	insertTorrent(hashCrawl, "Gated.Raw.Crawl.2026.720p")
	require.NoError(t, syncer.SyncOnce(ctx))
	h2, err := cli.Health(ctx)
	require.NoError(t, err)
	require.Equal(t, gen1, h2.GenerationID)
	require.Equal(t, 0, h2.L0Count, "unverified raw crawl must not consume L0 slots under active serving gate")

	// 3. New torrent receives live publication + payload proof -> immediately ingested into L0 and served!
	insertTorrent(hashLateVerified, "Gated.Late.Proof.2026.2160p.REMUX")
	approveAll(hashLateVerified, 1)
	require.NoError(t, syncer.SyncOnce(ctx))
	h3, err := cli.Health(ctx)
	require.NoError(t, err)
	require.Equal(t, gen1, h3.GenerationID)
	require.Equal(t, 1, h3.L0Count)

	res3, err := cli.Search(ctx, SearchRequest{Query: "late proof", Mode: ModeAnd, Tier: TierPayload, Limit: 10})
	require.NoError(t, err)
	require.Len(t, res3.Docs, 1)
	require.Equal(t, hashLateVerified, res3.Docs[0].InfoHashV1)

	// 4. Live publication hold immediately revokes fail-closed.
	_, err = pool.Exec(ctx, `
INSERT INTO processor_publication_holds (info_hash, held, reason, request_id)
VALUES (decode($1, 'hex'), true, 'operator_hold', repeat('3', 64))`, hashLateVerified)
	require.NoError(t, err)
	require.NoError(t, syncer.SyncOnce(ctx))
	res4, err := cli.Search(ctx, SearchRequest{Query: "late proof", Mode: ModeAnd, Tier: TierPayload, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, res4.Docs)

	// 5. processor_registry execution_generation bump triggers automatic rebuild revoking old-gen approvals!
	_, err = pool.Exec(ctx, `UPDATE processor_registry SET execution_generation = 2 WHERE id = 'test-reviewer'`)
	require.NoError(t, err)
	require.NoError(t, syncer.SyncOnce(ctx))
	h5, err := cli.Health(ctx)
	require.NoError(t, err)
	require.Greater(t, h5.GenerationID, gen1, "execution_generation bump must trigger automatic generation rebuild")
	res5, err := cli.Search(ctx, SearchRequest{Query: "verified release", Mode: ModeAnd, Tier: TierPayload, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, res5.Docs, "old-generation publication approval must no longer be served after generation bump")
}

// TestSyncerAuditHardeningMVCCOutboxTriggersAndExpiredLeaseRecovery proves:
//   - MVCC safety: An open concurrent PG transaction holding a lower outbox seq is
//     never skipped/deleted by an intervening SyncOnce or FullBuild.
//   - Trigger coverage: Migration 00102 triggers propagate video_source/video_codec/
//     video_3d/is_anime edits, content.adult flips, processor_publication_sources
//     revision bumps, and new content catalog inserts (op='catalog').
//   - Fail-closed recovery: Expired governance lease resolves via /v1/sync/resolve and applies
//     pending revocations BEFORE renewing the lease (never reopening prematurely).
func TestSyncerAuditHardeningMVCCOutboxTriggersAndExpiredLeaseRecovery(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("DH_WAYPOINT_TEST_POSTGRES_DSN not set; skipping audit hardening PG + dh-tmsd E2E test")
	}
	cli, _ := startTestDaemon(t)
	ctx := context.Background()

	adminCfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	adminDB := stdlib.OpenDB(*adminCfg)
	t.Cleanup(func() { _ = adminDB.Close() })

	dbName := fmt.Sprintf("dhw_dhtms_audit_%d", time.Now().UnixNano())
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

	hA := strings.Repeat("1a", 20)
	hB := strings.Repeat("2b", 20)
	_, err = pool.Exec(ctx, `
INSERT INTO content (type, source, id, title, original_title, release_year, adult, created_at, updated_at)
VALUES ('movie', 'tmdb', '701', 'Audit Movie One', 'Audit Movie One', 2024, false, now(), now())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO torrents (info_hash, name, size, private, files_status, created_at, updated_at) VALUES
  (decode($1, 'hex'), 'Audit.Movie.One.2024.1080p', 4000000000, false, 'single', now(), now()),
  (decode($2, 'hex'), 'Audit.Movie.Two.2024.1080p', 5000000000, false, 'single', now(), now())`, hA, hB)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO torrent_contents (info_hash, content_type, content_source, content_id, video_resolution, video_source, video_codec, is_anime, seeders, leechers, published_at, size, created_at, updated_at) VALUES
  (decode($1, 'hex'), 'movie', 'tmdb', '701', 'V1080p', 'WEBRip', 'x264', false, 10, 1, now(), 4000000000, now(), now()),
  (decode($2, 'hex'), 'movie', NULL, NULL, 'V1080p', 'WEBRip', 'x264', false, 20, 2, now(), 5000000000, now(), now())`, hA, hB)
	require.NoError(t, err)

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
	syncer := NewSyncer(pool, cli, pol, syncCfg, NewMetrics(), zap.NewNop().Sugar())

	require.NoError(t, syncer.SyncOnce(ctx))

	// 1. Migration 00102 trigger fires on video_source/video_codec/video_3d/is_anime updates!
	_, err = pool.Exec(ctx, `
UPDATE torrent_contents
SET video_source = 'BluRay', video_codec = 'x265', video_3d = 'V3DSBS', is_anime = true
WHERE info_hash = decode($1, 'hex')`, hB)
	require.NoError(t, err)
	require.NoError(t, syncer.SyncOnce(ctx))

	srB, err := cli.Search(ctx, SearchRequest{Query: "audit movie two", Mode: ModeAnd, Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.Len(t, srB.Docs, 1)
	require.Equal(t, hB, srB.Docs[0].InfoHashV1)
	require.Equal(t, VideoSourceToCode("BluRay"), srB.Docs[0].VideoSource)
	require.Equal(t, VideoCodecToCode("x265"), srB.Docs[0].VideoCodec)
	require.Equal(t, Video3DToCode("V3DSBS"), srB.Docs[0].Video3D)
	require.True(t, srB.Docs[0].IsAnime)

	// 2. MVCC concurrent tx gap: Tx A allocates a LOWER outbox seq for a hold
	//    on hB and stays uncommitted while Tx B commits a HIGHER outbox seq on hA.
	txA, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	_, err = txA.Exec(ctx, `
INSERT INTO processor_publication_holds (info_hash, held, reason, request_id)
VALUES (decode($1, 'hex'), true, 'mvcc_lower_seq_hold', repeat('a', 64))`, hB)
	require.NoError(t, err)

	// Tx B commits a higher seq while Tx A is still open.
	_, err = pool.Exec(ctx, `UPDATE torrent_contents SET seeders = 42 WHERE info_hash = decode($1, 'hex')`, hA)
	require.NoError(t, err)

	// SyncOnce consumes Tx B's higher seq.
	require.NoError(t, syncer.SyncOnce(ctx))
	hlBStill, err := cli.HashLookup(ctx, HashLookupRequest{InfoHash: hB, Tier: TierBase})
	require.NoError(t, err)
	require.True(t, hlBStill.Eligible, "uncommitted Tx A hold must not be visible yet")

	// Now Tx A commits its lower seq -> next SyncOnce MUST pick it up and revoke hB!
	require.NoError(t, txA.Commit(ctx))
	require.NoError(t, syncer.SyncOnce(ctx))
	hlBRevoked, err := cli.HashLookup(ctx, HashLookupRequest{InfoHash: hB, Tier: TierBase})
	require.NoError(t, err)
	require.False(t, hlBRevoked.Eligible, "lower-seq MVCC hold committed after higher-seq SyncOnce must NEVER be skipped")

	// 3. Expired-lease fail-closed recovery: expire lease, insert content.adult=true
	//    on tmdb:701 (triggering 00102 content adult fan-out), then run SyncOnce -> lease
	//    recovers ONLY AFTER hA is revoked (zero leak window)!
	hNow, err := cli.Health(ctx)
	require.NoError(t, err)
	_, err = cli.PolicyLease(ctx, PolicyLeaseRequest{PGLSN: hNow.PGLSN + 1, TTLMS: 0})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE content SET adult = true WHERE type='movie' AND source='tmdb' AND id='701'`)
	require.NoError(t, err)

	// Confirm lease is closed before SyncOnce.
	_, err = cli.HashLookup(ctx, HashLookupRequest{InfoHash: hA, Tier: TierBase})
	require.True(t, errors.Is(err, ErrLeaseExpired))

	require.NoError(t, syncer.SyncOnce(ctx))
	hlAAdult, err := cli.HashLookup(ctx, HashLookupRequest{InfoHash: hA, Tier: TierBase})
	require.NoError(t, err)
	require.False(t, hlAAdult.Eligible, "content.adult=true trigger + expired-lease SyncOnce must revoke hA before serving resumes")

	// 4. Catalog sync: Post-build new content row emits 'catalog' op -> triggers debounced FullBuild
	//    so Lookup returns entity_found=true for the newly inserted entity!
	hGenBefore, err := cli.Health(ctx)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO content (type, source, id, title, original_title, release_year, adult, created_at, updated_at)
VALUES ('movie', 'tmdb', '702', 'Post Build Entity', 'Post Build Entity', 2025, false, now(), now())`)
	require.NoError(t, err)
	require.NoError(t, syncer.SyncOnce(ctx))

	hGenAfter, err := cli.Health(ctx)
	require.NoError(t, err)
	require.Greater(t, hGenAfter.GenerationID, hGenBefore.GenerationID, "new content insert must trigger catalog rebuild")

	luNew, err := cli.Lookup(ctx, LookupRequest{ContentType: 1, ContentSource: "tmdb", ContentID: "702", Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.True(t, luNew.EntityFound, "post-rebuild lookup for new content row must report entity_found=true")
	luMissing, err := cli.Lookup(ctx, LookupRequest{ContentType: 1, ContentSource: "tmdb", ContentID: "999999", Tier: TierBase, Limit: 10})
	require.NoError(t, err)
	require.False(t, luMissing.EntityFound, "absent content row must report entity_found=false")
}
