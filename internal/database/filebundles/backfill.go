package filebundles

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const backfillCursorKey = "storage_file_bundles_backfill"

// BackfillOptions bounds one backfill or parity-verification run.
type BackfillOptions struct {
	BatchSize   int
	Pause       time.Duration
	MaxBatches  int
	VerifyOnly  bool
	PruneLegacy bool
	ResetCursor bool
}

// BackfillReport summarizes one Backfill run.
type BackfillReport struct {
	Batches          int      `json:"batches"`
	MigratedHashes   int64    `json:"migrated_hashes"`
	PrunedRows       int64    `json:"pruned_rows"`
	VerifiedHashes   int64    `json:"verified_hashes"`
	MismatchedHashes []string `json:"mismatched_hashes,omitempty"`
	Done             bool     `json:"done"`
	LastHash         string   `json:"last_hash,omitempty"`
}

// ParityReport summarizes one VerifyParity run.
type ParityReport struct {
	CheckedHashes    int64    `json:"checked_hashes"`
	MismatchedHashes []string `json:"mismatched_hashes,omitempty"`
}

// StatusReport reports legacy/bundle row counts and on-disk sizes.
type StatusReport struct {
	LegacyRows       int64  `json:"legacy_rows"`
	LegacyBytes      int64  `json:"legacy_bytes"`
	BundleRows       int64  `json:"bundle_rows"`
	BundleBytes      int64  `json:"bundle_bytes"`
	UnmigratedHashes int64  `json:"unmigrated_hashes"`
	Cursor           string `json:"cursor"`
}

type backfillCursor struct {
	LastHash string `json:"last_hash"`
	Done     bool   `json:"done"`
}

func normalizeOptions(opts BackfillOptions) BackfillOptions {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 2000
	}
	if opts.Pause == 0 {
		opts.Pause = 25 * time.Millisecond
	}
	return opts
}

func loadCursor(ctx context.Context, db Querier) (backfillCursor, error) {
	var raw string
	switch err := db.QueryRow(ctx, `SELECT value FROM key_values WHERE key=$1`, backfillCursorKey).Scan(&raw); {
	case errors.Is(err, pgx.ErrNoRows):
		return backfillCursor{}, nil
	case err != nil:
		return backfillCursor{}, fmt.Errorf("filebundles: load backfill cursor: %w", err)
	}
	var cur backfillCursor
	if err := json.Unmarshal([]byte(raw), &cur); err != nil {
		return backfillCursor{}, fmt.Errorf("filebundles: decode backfill cursor: %w", err)
	}
	return cur, nil
}

func saveCursor(ctx context.Context, db Querier, cur backfillCursor) error {
	value, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `INSERT INTO key_values(key,value,created_at,updated_at) VALUES($1,$2,now(),now())
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		backfillCursorKey, string(value)); err != nil {
		return fmt.Errorf("filebundles: save backfill cursor: %w", err)
	}
	return nil
}

// ResetBackfillCursor deletes the persisted backfill cursor.
func ResetBackfillCursor(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `DELETE FROM key_values WHERE key=$1`, backfillCursorKey); err != nil {
		return fmt.Errorf("filebundles: reset backfill cursor: %w", err)
	}
	return nil
}

type bundleRow struct {
	infoHash   []byte
	indexes    []int32
	paths      []string
	sizes      []int64
	extensions []*string
}

type legacyRow struct {
	index     int32
	path      string
	size      int64
	extension *string
}

func equalExtension(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// parityMismatches compares legacy rows against bundles for hashes and
// returns the hex hashes whose (index,path,size,extension) sequences differ.
func parityMismatches(ctx context.Context, db Querier, hashes [][]byte) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	legacy := make(map[string][]legacyRow, len(hashes))
	rows, err := db.Query(ctx,
		`SELECT info_hash,"index",path,size,extension FROM (SELECT DISTINCT ON (info_hash,path) info_hash,"index",path,size,extension FROM torrent_files WHERE info_hash=ANY($1) ORDER BY info_hash,path,"index") d ORDER BY info_hash,"index",path`,
		hashes)
	if err != nil {
		return nil, fmt.Errorf("filebundles: load legacy rows for parity: %w", err)
	}
	for rows.Next() {
		var hash []byte
		var r legacyRow
		if err := rows.Scan(&hash, &r.index, &r.path, &r.size, &r.extension); err != nil {
			rows.Close()
			return nil, fmt.Errorf("filebundles: scan legacy row for parity: %w", err)
		}
		legacy[string(hash)] = append(legacy[string(hash)], r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("filebundles: load legacy rows for parity: %w", err)
	}
	rows.Close()

	bundles := make(map[string]bundleRow, len(hashes))
	brows, err := db.Query(ctx,
		`SELECT info_hash,indexes,paths,sizes,extensions FROM torrent_file_bundles WHERE info_hash=ANY($1)`, hashes)
	if err != nil {
		return nil, fmt.Errorf("filebundles: load bundles for parity: %w", err)
	}
	for brows.Next() {
		var b bundleRow
		if err := brows.Scan(&b.infoHash, &b.indexes, &b.paths, &b.sizes, &b.extensions); err != nil {
			brows.Close()
			return nil, fmt.Errorf("filebundles: scan bundle for parity: %w", err)
		}
		bundles[string(b.infoHash)] = b
	}
	if err := brows.Err(); err != nil {
		brows.Close()
		return nil, fmt.Errorf("filebundles: load bundles for parity: %w", err)
	}
	brows.Close()

	var mismatched []string
	for _, hash := range hashes {
		want := legacy[string(hash)]
		b, ok := bundles[string(hash)]
		if !ok {
			mismatched = append(mismatched, hex.EncodeToString(hash))
			continue
		}
		bad := len(want) != len(b.paths) || len(b.indexes) != len(want) || len(b.sizes) != len(want) || len(b.extensions) != len(want)
		if !bad {
			for i, w := range want {
				if b.indexes[i] != w.index || b.paths[i] != w.path || b.sizes[i] != w.size || !equalExtension(b.extensions[i], w.extension) {
					bad = true
					break
				}
			}
		}
		if bad {
			mismatched = append(mismatched, hex.EncodeToString(hash))
		}
	}
	return mismatched, nil
}

// VerifyParity checks every bundled hash that still has legacy rows.
func VerifyParity(ctx context.Context, pool *pgxpool.Pool, opts BackfillOptions) (ParityReport, error) {
	opts = normalizeOptions(opts)
	var report ParityReport
	last := []byte{}
	for {
		rows, err := pool.Query(ctx,
			`SELECT b.info_hash FROM torrent_file_bundles b WHERE EXISTS(SELECT 1 FROM torrent_files f WHERE f.info_hash=b.info_hash) AND b.info_hash>$1 ORDER BY b.info_hash LIMIT $2`,
			last, opts.BatchSize)
		if err != nil {
			return report, fmt.Errorf("filebundles: select parity hashes: %w", err)
		}
		var batch [][]byte
		for rows.Next() {
			var h []byte
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return report, fmt.Errorf("filebundles: scan parity hash: %w", err)
			}
			batch = append(batch, h)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return report, fmt.Errorf("filebundles: select parity hashes: %w", err)
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		mismatched, err := parityMismatches(ctx, pool, batch)
		if err != nil {
			return report, err
		}
		report.CheckedHashes += int64(len(batch))
		for _, h := range mismatched {
			if len(report.MismatchedHashes) < 100 {
				report.MismatchedHashes = append(report.MismatchedHashes, h)
			}
		}
		last = batch[len(batch)-1]
		if len(batch) < opts.BatchSize {
			break
		}
		if opts.Pause > 0 {
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			case <-time.After(opts.Pause):
			}
		}
	}
	return report, nil
}

// Status reports row counts, relation sizes and the raw backfill cursor.
func Status(ctx context.Context, pool *pgxpool.Pool) (StatusReport, error) {
	var report StatusReport
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM torrent_files`).Scan(&report.LegacyRows); err != nil {
		return report, fmt.Errorf("filebundles: count legacy rows: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT pg_total_relation_size('torrent_files')`).Scan(&report.LegacyBytes); err != nil {
		return report, fmt.Errorf("filebundles: size torrent_files: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM torrent_file_bundles`).Scan(&report.BundleRows); err != nil {
		return report, fmt.Errorf("filebundles: count bundle rows: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT pg_total_relation_size('torrent_file_bundles')`).Scan(&report.BundleBytes); err != nil {
		return report, fmt.Errorf("filebundles: size torrent_file_bundles: %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM torrents t WHERE NOT EXISTS(SELECT 1 FROM torrent_file_bundles b WHERE b.info_hash=t.info_hash) AND EXISTS(SELECT 1 FROM torrent_files f WHERE f.info_hash=t.info_hash)`).
		Scan(&report.UnmigratedHashes); err != nil {
		return report, fmt.Errorf("filebundles: count unmigrated hashes: %w", err)
	}
	switch err := pool.QueryRow(ctx, `SELECT value FROM key_values WHERE key=$1`, backfillCursorKey).Scan(&report.Cursor); {
	case errors.Is(err, pgx.ErrNoRows):
		report.Cursor = ""
	case err != nil:
		return report, fmt.Errorf("filebundles: load backfill cursor: %w", err)
	}
	return report, nil
}

// Backfill copies legacy torrent_files rows into torrent_file_bundles in
// keyset batches, verifying parity inside each batch transaction before
// optionally pruning the legacy rows.
func Backfill(ctx context.Context, pool *pgxpool.Pool, opts BackfillOptions) (BackfillReport, error) {
	opts = normalizeOptions(opts)
	var report BackfillReport
	if opts.VerifyOnly {
		parity, err := VerifyParity(ctx, pool, opts)
		if err != nil {
			return report, err
		}
		report.VerifiedHashes = parity.CheckedHashes
		report.MismatchedHashes = parity.MismatchedHashes
		return report, nil
	}
	if opts.ResetCursor {
		if err := ResetBackfillCursor(ctx, pool); err != nil {
			return report, err
		}
	}
	cur, err := loadCursor(ctx, pool)
	if err != nil {
		return report, err
	}
	last := []byte{}
	if cur.LastHash != "" {
		last, err = hex.DecodeString(cur.LastHash)
		if err != nil {
			return report, fmt.Errorf("filebundles: decode cursor last hash: %w", err)
		}
	}
	report.LastHash = cur.LastHash

	for {
		if opts.MaxBatches > 0 && report.Batches >= opts.MaxBatches {
			break
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return report, fmt.Errorf("filebundles: begin backfill batch: %w", err)
		}
		batch, migrated, pruned, verified, done, batchErr := backfillBatch(ctx, tx, last, opts)
		if batchErr != nil {
			_ = tx.Rollback(ctx)
			return report, batchErr
		}
		if len(batch) > 0 {
			last = batch[len(batch)-1]
			report.LastHash = hex.EncodeToString(last)
		}
		if err := saveCursor(ctx, tx, backfillCursor{LastHash: report.LastHash, Done: done}); err != nil {
			_ = tx.Rollback(ctx)
			return report, err
		}
		if err := tx.Commit(ctx); err != nil {
			return report, fmt.Errorf("filebundles: commit backfill batch: %w", err)
		}
		report.Batches++
		report.MigratedHashes += migrated
		report.PrunedRows += pruned
		report.VerifiedHashes += verified
		report.Done = done
		if done {
			break
		}
		if opts.Pause > 0 {
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			case <-time.After(opts.Pause):
			}
		}
	}
	return report, nil
}

func backfillBatch(ctx context.Context, tx pgx.Tx, last []byte, opts BackfillOptions) (batch [][]byte, migrated, pruned, verified int64, done bool, err error) {
	if _, err := tx.Exec(ctx, `SELECT set_config('dh_waypoint.suppress_publication_trigger','on',true)`); err != nil {
		return nil, 0, 0, 0, false, fmt.Errorf("filebundles: suppress publication trigger: %w", err)
	}
	rows, err := tx.Query(ctx,
		`SELECT t.info_hash FROM torrents t WHERE t.info_hash>$1 AND EXISTS(SELECT 1 FROM torrent_files f WHERE f.info_hash=t.info_hash) ORDER BY t.info_hash LIMIT $2`,
		last, opts.BatchSize)
	if err != nil {
		return nil, 0, 0, 0, false, fmt.Errorf("filebundles: select backfill hashes: %w", err)
	}
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return nil, 0, 0, 0, false, fmt.Errorf("filebundles: scan backfill hash: %w", err)
		}
		batch = append(batch, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, 0, 0, false, fmt.Errorf("filebundles: select backfill hashes: %w", err)
	}
	rows.Close()
	if len(batch) == 0 {
		return batch, 0, 0, 0, true, nil
	}
	done = len(batch) < opts.BatchSize

	tag, err := tx.Exec(ctx,
		`INSERT INTO torrent_file_bundles(info_hash,indexes,paths,sizes,extensions) SELECT d.info_hash, array_agg(d."index" ORDER BY d."index",d.path), array_agg(d.path ORDER BY d."index",d.path), array_agg(d.size ORDER BY d."index",d.path), array_agg(d.extension ORDER BY d."index",d.path) FROM (SELECT DISTINCT ON (info_hash,path) info_hash,"index",path,size,extension FROM torrent_files WHERE info_hash=ANY($1) ORDER BY info_hash,path,"index") d GROUP BY d.info_hash ON CONFLICT(info_hash) DO NOTHING`,
		batch)
	if err != nil {
		return batch, 0, 0, 0, false, fmt.Errorf("filebundles: insert bundles: %w", err)
	}
	migrated = tag.RowsAffected()

	mismatched, err := parityMismatches(ctx, tx, batch)
	if err != nil {
		return batch, 0, 0, 0, false, err
	}
	if len(mismatched) > 0 {
		return batch, 0, 0, 0, false, fmt.Errorf("filebundles: bundle parity mismatch for hashes %v", mismatched)
	}
	verified = int64(len(batch))

	if opts.PruneLegacy {
		tag, err := tx.Exec(ctx, `DELETE FROM torrent_files WHERE info_hash=ANY($1)`, batch)
		if err != nil {
			return batch, 0, 0, 0, false, fmt.Errorf("filebundles: prune legacy rows: %w", err)
		}
		pruned = tag.RowsAffected()
	}
	return batch, migrated, pruned, verified, done, nil
}
