package filebundles_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/database/filebundles"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func bundleTestDB(t *testing.T) (*pgxpool.Pool, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to a disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	schema := fmt.Sprintf("filebundles_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		cleanup, e := pgx.Connect(context.Background(), dsn)
		if e == nil {
			_, _ = cleanup.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
			_ = cleanup.Close(context.Background())
		}
	})
	return pool, gdb
}

func seedTorrentRow(t *testing.T, pool *pgxpool.Pool, hash protocol.InfoHash) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,'Synthetic.Bundle',1015,false,'multi',3,now(),now())`, hash.Bytes())
	require.NoError(t, err)
}

func syntheticFiles(hash protocol.InfoHash) []model.TorrentFile {
	return []model.TorrentFile{
		{InfoHash: hash, Index: 0, Path: "Bundle/film.mkv", Size: 1000},
		{InfoHash: hash, Index: 1, Path: "Bundle/subs.srt", Size: 10},
		{InfoHash: hash, Index: 2, Path: "README", Size: 5},
	}
}

func fileKeys(files []model.TorrentFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		ext := "<null>"
		if f.Extension.Valid {
			ext = f.Extension.String
		}
		out[i] = fmt.Sprintf("%d|%s|%d|%s", f.Index, f.Path, f.Size, ext)
	}
	return out
}

// legacy, dual and compact writes must all read back as the identical
// effective file list, with no duplicates in dual mode.
func TestPersistModesAndLoadParity(t *testing.T) {
	pool, gdb := bundleTestDB(t)
	ctx := context.Background()
	legacyH := protocol.InfoHash{0x11}
	dualH := protocol.InfoHash{0x22}
	compactH := protocol.InfoHash{0x33}
	pgxH := protocol.InfoHash{0x44}
	for _, h := range []protocol.InfoHash{legacyH, dualH, compactH, pgxH} {
		seedTorrentRow(t, pool, h)
	}
	ptr := func(h protocol.InfoHash) []*model.TorrentFile {
		files := syntheticFiles(h)
		out := make([]*model.TorrentFile, len(files))
		for i := range files {
			out[i] = &files[i]
		}
		return out
	}
	require.NoError(t, gdb.Transaction(func(tx *gorm.DB) error {
		if err := filebundles.PersistTorrentFiles(ctx, tx, filebundles.ModeLegacy, ptr(legacyH)); err != nil {
			return err
		}
		if err := filebundles.PersistTorrentFiles(ctx, tx, filebundles.ModeDual, ptr(dualH)); err != nil {
			return err
		}
		return filebundles.PersistTorrentFiles(ctx, tx, filebundles.ModeCompact, ptr(compactH))
	}))
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, filebundles.PersistTorrentFilesPgx(ctx, tx, filebundles.ModeCompact, syntheticFiles(pgxH)))
	require.NoError(t, tx.Commit(ctx))

	loaded, err := filebundles.LoadFilesByInfoHashes(ctx, pool, []protocol.InfoHash{legacyH, dualH, compactH, pgxH})
	require.NoError(t, err)
	want := fileKeys(loaded[legacyH])
	require.Len(t, want, 3)
	require.Equal(t, "2|README|5|<null>", want[2], "extension-free file keeps NULL extension in every mode")
	for _, h := range []protocol.InfoHash{dualH, compactH, pgxH} {
		require.Equal(t, want, fileKeys(loaded[h]), "mode parity for %s", h)
	}
	counts := func(sql string, h protocol.InfoHash) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, sql, h.Bytes()).Scan(&n))
		return n
	}
	require.Equal(t, 3, counts(`SELECT count(*) FROM torrent_files WHERE info_hash=$1`, legacyH))
	require.Zero(t, counts(`SELECT count(*) FROM torrent_file_bundles WHERE info_hash=$1`, legacyH))
	require.Equal(t, 3, counts(`SELECT count(*) FROM torrent_files WHERE info_hash=$1`, dualH))
	require.Equal(t, 1, counts(`SELECT count(*) FROM torrent_file_bundles WHERE info_hash=$1`, dualH))
	require.Zero(t, counts(`SELECT count(*) FROM torrent_files WHERE info_hash=$1`, compactH))
	require.Equal(t, 1, counts(`SELECT count(*) FROM torrent_file_bundles WHERE info_hash=$1`, compactH))
}

// Bundles are write-once and duplicate paths collapse to the first by index.
func TestBundleWriteOnceAndDedup(t *testing.T) {
	pool, _ := bundleTestDB(t)
	ctx := context.Background()
	h := protocol.InfoHash{0x55}
	seedTorrentRow(t, pool, h)
	files := []model.TorrentFile{
		{InfoHash: h, Index: 1, Path: "dup.mkv", Size: 200},
		{InfoHash: h, Index: 0, Path: "dup.mkv", Size: 100},
		{InfoHash: h, Index: 2, Path: "other.srt", Size: 5},
	}
	require.NoError(t, filebundles.UpsertBundles(ctx, pool, map[protocol.InfoHash][]model.TorrentFile{h: files}))
	loaded, err := filebundles.LoadFilesByInfoHashes(ctx, pool, []protocol.InfoHash{h})
	require.NoError(t, err)
	require.Equal(t, []string{"0|dup.mkv|100|mkv", "2|other.srt|5|srt"}, fileKeys(loaded[h]))
	second := []model.TorrentFile{{InfoHash: h, Index: 0, Path: "dup.mkv", Size: 999}}
	require.NoError(t, filebundles.UpsertBundles(ctx, pool, map[protocol.InfoHash][]model.TorrentFile{h: second}))
	loaded, err = filebundles.LoadFilesByInfoHashes(ctx, pool, []protocol.InfoHash{h})
	require.NoError(t, err)
	require.Equal(t, []string{"0|dup.mkv|100|mkv", "2|other.srt|5|srt"}, fileKeys(loaded[h]), "existing bundle must not be overwritten")
}

func publicationState(t *testing.T, pool *pgxpool.Pool) (map[string]int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT info_hash,revision FROM processor_publication_sources ORDER BY info_hash`)
	require.NoError(t, err)
	defer rows.Close()
	revisions := map[string]int64{}
	for rows.Next() {
		var h []byte
		var rev int64
		require.NoError(t, rows.Scan(&h, &rev))
		revisions[hex.EncodeToString(h)] = rev
	}
	require.NoError(t, rows.Err())
	var pendingCount, pendingSum int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*),coalesce(sum(source_revision),0) FROM processor_publication_pending`).Scan(&pendingCount, &pendingSum))
	return revisions, pendingCount, pendingSum
}

// Online backfill + prune moves zero publication revisions and inserts zero
// pending rows, then resumes as a no-op.
func TestBackfillSuppressesPublicationAndPrunes(t *testing.T) {
	pool, _ := bundleTestDB(t)
	ctx := context.Background()
	var hashes []protocol.InfoHash
	for i := byte(1); i <= 5; i++ {
		h := protocol.InfoHash{i}
		hashes = append(hashes, h)
		seedTorrentRow(t, pool, h)
		for _, f := range syntheticFiles(h) {
			_, err := pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,$2,$3,$4,now(),now())`, h.Bytes(), f.Index, f.Path, f.Size)
			require.NoError(t, err)
		}
	}
	revisions0, pendingCount0, pendingSum0 := publicationState(t, pool)
	require.Len(t, revisions0, 5)

	report, err := filebundles.Backfill(ctx, pool, filebundles.BackfillOptions{BatchSize: 2, Pause: time.Millisecond, PruneLegacy: true})
	require.NoError(t, err)
	require.True(t, report.Done)
	require.EqualValues(t, 5, report.MigratedHashes)
	require.EqualValues(t, 15, report.PrunedRows)
	require.EqualValues(t, 5, report.VerifiedHashes)
	require.Equal(t, 3, report.Batches)

	revisions1, pendingCount1, pendingSum1 := publicationState(t, pool)
	require.Equal(t, revisions0, revisions1, "backfill must not bump any publication revision")
	require.Equal(t, pendingCount0, pendingCount1, "backfill must not insert pending rows")
	require.Equal(t, pendingSum0, pendingSum1)

	loaded, err := filebundles.LoadFilesByInfoHashes(ctx, pool, hashes)
	require.NoError(t, err)
	for _, h := range hashes {
		require.Equal(t, []string{"0|Bundle/film.mkv|1000|mkv", "1|Bundle/subs.srt|10|srt", "2|README|5|<null>"}, fileKeys(loaded[h]))
	}
	var legacyLeft int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM torrent_files`).Scan(&legacyLeft))
	require.Zero(t, legacyLeft)

	again, err := filebundles.Backfill(ctx, pool, filebundles.BackfillOptions{BatchSize: 2, Pause: time.Millisecond, PruneLegacy: true})
	require.NoError(t, err)
	require.True(t, again.Done)
	require.Zero(t, again.MigratedHashes)
	require.Zero(t, again.PrunedRows)

	status, err := filebundles.Status(ctx, pool)
	require.NoError(t, err)
	require.EqualValues(t, 0, status.LegacyRows)
	require.EqualValues(t, 5, status.BundleRows)
	require.EqualValues(t, 0, status.UnmigratedHashes)
	require.NotEmpty(t, status.Cursor)
}

// Parity verification flags a corrupted bundle and only that bundle.
func TestVerifyParityDetectsMismatch(t *testing.T) {
	pool, _ := bundleTestDB(t)
	ctx := context.Background()
	good := protocol.InfoHash{0x61}
	bad := protocol.InfoHash{0x62}
	for _, h := range []protocol.InfoHash{good, bad} {
		seedTorrentRow(t, pool, h)
		files := syntheticFiles(h)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, filebundles.PersistTorrentFilesPgx(ctx, tx, filebundles.ModeDual, files))
		require.NoError(t, tx.Commit(ctx))
	}
	parity, err := filebundles.VerifyParity(ctx, pool, filebundles.BackfillOptions{BatchSize: 10, Pause: time.Millisecond})
	require.NoError(t, err)
	require.EqualValues(t, 2, parity.CheckedHashes)
	require.Empty(t, parity.MismatchedHashes)

	_, err = pool.Exec(ctx, `UPDATE torrent_file_bundles SET paths=ARRAY['Bundle/film.mkv','Bundle/subs.srt','TAMPERED'] WHERE info_hash=$1`, bad.Bytes())
	require.NoError(t, err)
	parity, err = filebundles.VerifyParity(ctx, pool, filebundles.BackfillOptions{BatchSize: 10, Pause: time.Millisecond})
	require.NoError(t, err)
	require.Equal(t, []string{bad.String()}, parity.MismatchedHashes)
	report, err := filebundles.Backfill(ctx, pool, filebundles.BackfillOptions{VerifyOnly: true, BatchSize: 10, Pause: time.Millisecond})
	require.NoError(t, err)
	require.EqualValues(t, 2, report.VerifiedHashes)
	require.Equal(t, []string{bad.String()}, report.MismatchedHashes)
}

// Measured size check: 200 torrents x 40 files occupy substantially less
// space as bundle rows than as legacy per-file rows with their indexes.
func TestBundleStorageSavings(t *testing.T) {
	pool, _ := bundleTestDB(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at)
SELECT decode(md5('savings-torrent-'||i::text)||substring(md5('savings-extra-'||i::text),1,8),'hex'),
       'Synthetic.Collection.'||i, 40000000, false, 'multi', 40, now(), now()
FROM generate_series(1,200) i;
INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)
SELECT t.info_hash, f-1,
       'Synthetic.Collection.Season.01/Synthetic.Collection.S01E'||lpad(f::text,2,'0')||'.1080p.WEB-DL.H264-SYNTHETIC.mkv',
       1000000+f, now(), now()
FROM torrents t CROSS JOIN generate_series(1,40) f`)
	require.NoError(t, err)
	var legacyBytes int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT pg_total_relation_size('torrent_files')`).Scan(&legacyBytes))

	report, err := filebundles.Backfill(ctx, pool, filebundles.BackfillOptions{BatchSize: 50, Pause: time.Millisecond})
	require.NoError(t, err)
	require.True(t, report.Done)
	require.EqualValues(t, 200, report.MigratedHashes)
	var bundleBytes int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT pg_total_relation_size('torrent_file_bundles')`).Scan(&bundleBytes))
	t.Logf("storage: legacy torrent_files=%d bytes, torrent_file_bundles=%d bytes, ratio=%.1fx smaller",
		legacyBytes, bundleBytes, float64(legacyBytes)/float64(bundleBytes))
	require.Less(t, bundleBytes*2, legacyBytes, "bundles must be at least 2x smaller on this synthetic batch")
}

// Bulk multi-row upserts span multiple chunks (>250 hashes) identically across
// pgxpool (UpsertBundles) and GORM (PersistTorrentFiles) while preserving
// write-once conflict semantics and NULL extensions.
func TestBulkUpsertBundlesMultiChunkPostgres(t *testing.T) {
	pool, gdb := bundleTestDB(t)
	ctx := context.Background()

	mkHash := func(prefix byte, n int) protocol.InfoHash {
		var h protocol.InfoHash
		h[0] = prefix
		h[1] = byte(n >> 8)
		h[2] = byte(n)
		return h
	}

	// Seed 600 torrents for UpsertBundles (3 chunks: 250 + 250 + 100).
	const pgxCount = 600
	pgxHashes := make([]protocol.InfoHash, pgxCount)
	byHash := make(map[protocol.InfoHash][]model.TorrentFile, pgxCount)
	batch := &pgx.Batch{}
	for i := range pgxCount {
		h := mkHash(0xA1, i+1)
		pgxHashes[i] = h
		batch.Queue(`INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,'Bulk.Pgx',1015,false,'multi',3,now(),now())`, h.Bytes())
		byHash[h] = syntheticFiles(h)
	}
	br := pool.SendBatch(ctx, batch)
	for range pgxCount {
		_, err := br.Exec()
		require.NoError(t, err)
	}
	require.NoError(t, br.Close())

	// Pre-insert one bundle with distinct data to verify ON CONFLICT DO NOTHING
	// inside a multi-row chunk preserves existing rows.
	sentinel := pgxHashes[275]
	require.NoError(t, filebundles.UpsertBundles(ctx, pool, map[protocol.InfoHash][]model.TorrentFile{
		sentinel: {{InfoHash: sentinel, Index: 0, Path: "keep.mkv", Size: 777}},
	}))

	require.NoError(t, filebundles.UpsertBundles(ctx, pool, byHash))
	loaded, err := filebundles.LoadFilesByInfoHashes(ctx, pool, pgxHashes)
	require.NoError(t, err)
	require.Len(t, loaded, pgxCount)
	require.Equal(t, []string{"0|keep.mkv|777|mkv"}, fileKeys(loaded[sentinel]))
	require.Equal(t, []string{"0|Bundle/film.mkv|1000|mkv", "1|Bundle/subs.srt|10|srt", "2|README|5|<null>"}, fileKeys(loaded[pgxHashes[0]]))
	require.Equal(t, []string{"0|Bundle/film.mkv|1000|mkv", "1|Bundle/subs.srt|10|srt", "2|README|5|<null>"}, fileKeys(loaded[pgxHashes[pgxCount-1]]))

	// Seed 300 torrents for GORM PersistTorrentFiles (2 chunks: 250 + 50).
	const gormCount = 300
	gormHashes := make([]protocol.InfoHash, gormCount)
	var gormFiles []*model.TorrentFile
	gBatch := &pgx.Batch{}
	for i := range gormCount {
		h := mkHash(0xB2, i+1)
		gormHashes[i] = h
		gBatch.Queue(`INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at) VALUES($1,'Bulk.Gorm',1015,false,'multi',3,now(),now())`, h.Bytes())
		for _, f := range syntheticFiles(h) {
			fc := f
			gormFiles = append(gormFiles, &fc)
		}
	}
	gbr := pool.SendBatch(ctx, gBatch)
	for range gormCount {
		_, err := gbr.Exec()
		require.NoError(t, err)
	}
	require.NoError(t, gbr.Close())

	require.NoError(t, gdb.Transaction(func(tx *gorm.DB) error {
		return filebundles.PersistTorrentFiles(ctx, tx, filebundles.ModeCompact, gormFiles)
	}))
	loadedGorm, err := filebundles.LoadFilesByInfoHashes(ctx, pool, gormHashes)
	require.NoError(t, err)
	require.Len(t, loadedGorm, gormCount)
	require.Equal(t, []string{"0|Bundle/film.mkv|1000|mkv", "1|Bundle/subs.srt|10|srt", "2|README|5|<null>"}, fileKeys(loadedGorm[gormHashes[299]]))
}

// StatusFast reports O(1) pg_class estimates without table scans, Progress
// streams batch updates, and closing Stop cleanly checkpoints after the active
// batch commits so a resumed run finishes cleanly.
func TestStatusFastProgressAndGracefulStopPostgres(t *testing.T) {
	pool, _ := bundleTestDB(t)
	ctx := context.Background()

	var hashes []protocol.InfoHash
	for i := byte(1); i <= 10; i++ {
		h := protocol.InfoHash{0xC0, i}
		hashes = append(hashes, h)
		seedTorrentRow(t, pool, h)
		for _, f := range syntheticFiles(h) {
			_, err := pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,$2,$3,$4,now(),now())`, h.Bytes(), f.Index, f.Path, f.Size)
			require.NoError(t, err)
		}
	}
	_, err := pool.Exec(ctx, `ANALYZE torrent_files, torrent_file_bundles`)
	require.NoError(t, err)

	fastBefore, err := filebundles.StatusFast(ctx, pool)
	require.NoError(t, err)
	require.True(t, fastBefore.Estimated)
	require.EqualValues(t, -1, fastBefore.UnmigratedHashes)
	require.EqualValues(t, 30, fastBefore.LegacyRows)
	require.Greater(t, fastBefore.LegacyBytes, int64(0))
	require.EqualValues(t, 0, fastBefore.BundleRows)

	stopCh := make(chan struct{})
	var updates []filebundles.BackfillProgress
	report1, err := filebundles.Backfill(ctx, pool, filebundles.BackfillOptions{
		BatchSize:        2,
		Pause:            time.Millisecond,
		PruneLegacy:      true,
		Stop:             stopCh,
		ProgressInterval: 0,
		Progress: func(p filebundles.BackfillProgress) {
			updates = append(updates, p)
			if p.Batches == 2 && !p.Interrupted {
				close(stopCh)
			}
		},
	})
	require.NoError(t, err)
	require.True(t, report1.Interrupted)
	require.False(t, report1.Done)
	require.Equal(t, 2, report1.Batches)
	require.EqualValues(t, 4, report1.MigratedHashes)
	require.EqualValues(t, 12, report1.PrunedRows)
	require.NotEmpty(t, report1.LastHash)
	require.GreaterOrEqual(t, len(updates), 2)
	require.True(t, updates[len(updates)-1].Interrupted)

	// Resume without resetting cursor: finishes remaining 6 hashes cleanly.
	var resumeUpdates []filebundles.BackfillProgress
	report2, err := filebundles.Backfill(ctx, pool, filebundles.BackfillOptions{
		BatchSize:        2,
		Pause:            time.Millisecond,
		PruneLegacy:      true,
		ProgressInterval: time.Hour, // final update still fires on completion
		Progress: func(p filebundles.BackfillProgress) {
			resumeUpdates = append(resumeUpdates, p)
		},
	})
	require.NoError(t, err)
	require.False(t, report2.Interrupted)
	require.True(t, report2.Done)
	require.EqualValues(t, 6, report2.MigratedHashes)
	require.EqualValues(t, 18, report2.PrunedRows)
	require.Len(t, resumeUpdates, 1)
	require.True(t, resumeUpdates[0].Done)

	_, err = pool.Exec(ctx, `ANALYZE torrent_files, torrent_file_bundles`)
	require.NoError(t, err)
	fastAfter, err := filebundles.StatusFast(ctx, pool)
	require.NoError(t, err)
	require.True(t, fastAfter.Estimated)
	require.EqualValues(t, 0, fastAfter.LegacyRows)
	require.EqualValues(t, 10, fastAfter.BundleRows)
	require.NotEmpty(t, fastAfter.Cursor)

	loaded, err := filebundles.LoadFilesByInfoHashes(ctx, pool, hashes)
	require.NoError(t, err)
	require.Len(t, loaded, 10)
}
