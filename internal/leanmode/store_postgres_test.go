package leanmode

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
)

func setupLeanTestDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `SELECT pg_advisory_lock(830583);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830583)`)
	require.NoError(t, err)

	schema := fmt.Sprintf("dh_waypoint_lean_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = admin.ExecContext(cleanup, "DROP SCHEMA "+quoted+" CASCADE")
	})

	cfg.RuntimeParams["search_path"] = schema + ",public"
	stdDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = stdDB.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, stdDB, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)

	// Verify migration 83 is cleanly reversible.
	_, err = provider.DownTo(ctx, 82)
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 83)
	require.NoError(t, err)

	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, ctx
}

func TestLeanModePostgresLifecycleAndInvariants(t *testing.T) {
	pool, ctx := setupLeanTestDB(t)
	store := NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	now := time.Now().UTC()
	old := now.Add(-120 * time.Hour)

	// Seed metadata_sources + content + 4 movie releases (1 best, 1 runner-up, 1 dominated loser, 1 protected loser)
	_, err := pool.Exec(ctx, `INSERT INTO metadata_sources (key, name, created_at, updated_at) VALUES ('tmdb', 'TMDB', $1, $1) ON CONFLICT DO NOTHING`, old)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO content (type, source, id, title, release_year, created_at, updated_at) VALUES ('movie', 'tmdb', '550', 'Fight Club', 1999, $1, $1)`, old)
	require.NoError(t, err)

	hWin := mkHash(0x11)
	hRun := mkHash(0x22)
	hLose := mkHash(0x33)
	hProt := mkHash(0x44)

	insertRel := func(h []byte, name, res, src, codec, grp string, size int64, seeders int, private bool) {
		_, err := pool.Exec(ctx, `INSERT INTO torrents (info_hash, name, size, private, files_status, files_count, created_at, updated_at) VALUES ($1,$2,$3,$4,'single',1,$5,$5)`,
			h, name, size, private, old)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_contents (info_hash, content_type, content_source, content_id, video_resolution, video_source, video_codec, release_group, size, seeders, published_at, created_at, updated_at) VALUES ($1,'movie','tmdb','550',$2,$3,$4,$5,$6,$7,$8,$8,$8)`,
			h, res, src, codec, grp, size, seeders, old)
		require.NoError(t, err)
	}
	insertRel(hWin, "Fight.Club.1999.1080p.BluRay.x265-NTb.mkv", "V1080p", "BluRay", "x265", "NTb", 4200<<20, 80, false)
	insertRel(hRun, "Fight.Club.1999.1080p.WEBDL.x264-FLUX.mkv", "V1080p", "WEBDL", "x264", "FLUX", 3800<<20, 50, false)
	insertRel(hLose, "Fight.Club.1999.720p.WEBRip.x264-LOW.mkv", "V720p", "WEBRip", "x264", "LOW", 1200<<20, 10, false)
	insertRel(hProt, "Fight.Club.1999.720p.WEBRip.x264-PRIV.mkv", "V720p", "WEBRip", "x264", "PRIV", 1100<<20, 5, true)

	// Record initial processor_publication_sources revisions for kept winners.
	var winRevBefore, runRevBefore int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, hWin).Scan(&winRevBefore))
	require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, hRun).Scan(&runRevBefore))

	prof, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "1080p-lean.yml"))
	require.NoError(t, err)

	// 1. Observe mode: computes winners + runner-up, deletes NOTHING, 0 revision bumps!
	repObs, err := store.Reconcile(ctx, prof, ReconcileOptions{Now: now})
	require.NoError(t, err)
	require.Equal(t, 1, repObs.TitlesEvaluated)
	require.Equal(t, 1, repObs.WinnersKept)
	require.Equal(t, 1, repObs.RunnerUpsKept)
	require.Equal(t, 1, repObs.WouldPruneCount)
	require.Equal(t, 1, repObs.ProtectedSkipped)
	require.Equal(t, 0, repObs.PrunedCount)

	var winRevAfter, runRevAfter int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, hWin).Scan(&winRevAfter))
	require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, hRun).Scan(&runRevAfter))
	require.Equal(t, winRevBefore, winRevAfter, "observe mode must NEVER bump kept winner publication revision")
	require.Equal(t, runRevBefore, runRevAfter, "observe mode must NEVER bump runner-up publication revision")

	// 2. Mode=prune without confirm_destructive MUST fail closed!
	profUnconfirmed := prof
	profUnconfirmed.Mode = ModePrune
	profUnconfirmed.Prune.ConfirmDestructive = false
	_, err = store.Reconcile(ctx, profUnconfirmed, ReconcileOptions{Now: now})
	require.ErrorIs(t, err, ErrDestructiveUnconfirmed)

	// 3. Mode=prune WITH confirm_destructive=true deletes ONLY hLose, keeps hWin, hRun, and hProt!
	profPrune := prof
	profPrune.Mode = ModePrune
	profPrune.Prune.ConfirmDestructive = true
	profPrune.Prune.MaxDeleteFractionPerCycle = 0.50
	repPrune, err := store.Reconcile(ctx, profPrune, ReconcileOptions{Now: now})
	require.NoError(t, err)
	require.Equal(t, 1, repPrune.PrunedCount)
	require.Equal(t, 1, repPrune.TombstonesWritten)
	require.Equal(t, 1, repPrune.ProtectedSkipped)

	// Verify hLose deleted, hWin/hRun/hProt intact, and kept revisions still 0 drift!
	var remaining int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrents`).Scan(&remaining))
	require.Equal(t, 3, remaining)
	require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, hWin).Scan(&winRevAfter))
	require.Equal(t, winRevBefore, winRevAfter, "prune mode must NEVER bump kept winner publication revision")

	// 4. Tombstone gate hits hLose under matching digest, misses under changed digest!
	store.SetActiveProfile(&profPrune, false)
	blocked, err := store.TombstonedSet(ctx, [][]byte{hWin, hLose})
	require.NoError(t, err)
	require.Contains(t, blocked, hex.EncodeToString(hLose))
	require.NotContains(t, blocked, hex.EncodeToString(hWin))

	profChanged := profPrune
	profChanged.Required.MinSeeders = 2
	store.SetActiveProfile(&profChanged, false)
	blockedAfterChange, err := store.TombstonedSet(ctx, [][]byte{hLose})
	require.NoError(t, err)
	require.Empty(t, blockedAfterChange, "changing YAML config digest must immediately un-block old tombstones")

	// 5. External deletion of rank-1 winner succeeds without FK cascade, immediately disappears from Status(),
	//    and promotes the standby runner-up while sweeping the orphaned lean_winners row on next reconcile.
	_, err = pool.Exec(ctx, `DELETE FROM torrents WHERE info_hash = $1`, hWin)
	require.NoError(t, err)
	stAfterDel, err := store.Status(ctx, profPrune.ID)
	require.NoError(t, err)
	require.Len(t, stAfterDel, 1)
	require.Equal(t, int64(0), stAfterDel[0].WinnersRank1)
	require.Equal(t, int64(1), stAfterDel[0].RunnerUps)

	repPromote, err := store.Reconcile(ctx, profPrune, ReconcileOptions{Now: now.Add(time.Minute)})
	require.NoError(t, err)
	require.Equal(t, 1, repPromote.WinnersKept)
	var promotedHash []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT info_hash FROM lean_winners WHERE profile_id = $1 AND rank = 1`, profPrune.ID).Scan(&promotedHash))
	require.Equal(t, hRun, promotedHash)
	var orphanCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM lean_winners WHERE info_hash = $1`, hWin).Scan(&orphanCount))
	require.Equal(t, 0, orphanCount)
}

func TestLeanModeHandlesNullBundleExtensionsAndServingCriteria(t *testing.T) {
	pool, ctx := setupLeanTestDB(t)
	store := NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	now := time.Now().UTC()
	old := now.Add(-120 * time.Hour)

	_, err := pool.Exec(ctx, `INSERT INTO metadata_sources (key, name, created_at, updated_at) VALUES ('tmdb', 'TMDB', $1, $1) ON CONFLICT DO NOTHING`, old)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO content (type, source, id, title, release_year, created_at, updated_at) VALUES ('movie', 'tmdb', '603', 'The Matrix', 1999, $1, $1)`, old)
	require.NoError(t, err)

	hBundle := mkHash(0x55)
	_, err = pool.Exec(ctx, `INSERT INTO torrents (info_hash, name, size, private, files_status, files_count, created_at, updated_at) VALUES ($1,'The.Matrix.1999.1080p.BluRay.x265-NTb',4500000000,false,'multi',3,$2,$2)`, hBundle, old)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO torrent_contents (info_hash, content_type, content_source, content_id, video_resolution, video_source, video_codec, release_group, size, seeders, published_at, created_at, updated_at) VALUES ($1,'movie','tmdb','603','V1080p','BluRay','x265','NTb',4500000000,60,$2,$2,$2)`, hBundle, old)
	require.NoError(t, err)
	// Real-world bundles can contain NULL in extensions[] for extensionless files like README.
	_, err = pool.Exec(ctx, `INSERT INTO torrent_file_bundles (info_hash, indexes, paths, sizes, extensions, created_at, updated_at) VALUES ($1,ARRAY[0,1,2],ARRAY['movie.mkv','README','movie.srt'],ARRAY[4499990000,5000,5000],ARRAY['mkv', NULL, 'srt']::text[],$2,$2)`, hBundle, old)
	require.NoError(t, err)

	prof, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "1080p-lean.yml"))
	require.NoError(t, err)

	rep, err := store.Reconcile(ctx, prof, ReconcileOptions{Now: now})
	require.NoError(t, err)
	require.Equal(t, 1, rep.WinnersKept)

	// Verify ExplainTitle surfaces single-clean-video and embedded-subtitle signals despite NULL extension element.
	expl, err := store.ExplainTitle(ctx, prof, "movie", "tmdb", "603", now)
	require.NoError(t, err)
	require.Len(t, expl.Slots, 1)
	require.Len(t, expl.Slots[0].Winners, 1)
	require.Equal(t, hBundle, expl.Slots[0].Winners[0].Candidate.InfoHash)
	require.Equal(t, 1, expl.Slots[0].Winners[0].Candidate.VideoFileCount)
	require.True(t, expl.Slots[0].Winners[0].Candidate.HasEmbeddedSubtitles)

	// Verify Status and ClearTombstones execute cleanly.
	st, err := store.Status(ctx, prof.ID)
	require.NoError(t, err)
	require.Len(t, st, 1)
	require.Equal(t, int64(1), st[0].WinnersRank1)

	cleared, err := store.ClearTombstones(ctx, prof.ID, "")
	require.NoError(t, err)
	require.Equal(t, int64(0), cleared)
}
