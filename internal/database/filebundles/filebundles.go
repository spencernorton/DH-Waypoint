// Package filebundles persists and loads per-torrent file lists in the
// compact torrent_file_bundles representation, with legacy torrent_files
// fallback through torrent_files_effective.
package filebundles

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WriteMode selects which representations new file lists are written to.
type WriteMode string

const (
	ModeCompact WriteMode = "compact"
	ModeDual    WriteMode = "dual"
	ModeLegacy  WriteMode = "legacy"
)

// ParseWriteMode validates a write-mode name.
func ParseWriteMode(s string) (WriteMode, error) {
	switch WriteMode(strings.TrimSpace(s)) {
	case ModeCompact:
		return ModeCompact, nil
	case ModeDual:
		return ModeDual, nil
	case ModeLegacy:
		return ModeLegacy, nil
	default:
		return "", fmt.Errorf("filebundles: invalid write mode %q (want compact, dual or legacy)", s)
	}
}

// WriteModeFromEnv reads STORAGE_FILE_BUNDLES_WRITE; empty means compact.
func WriteModeFromEnv() (WriteMode, error) {
	v := os.Getenv("STORAGE_FILE_BUNDLES_WRITE")
	if strings.TrimSpace(v) == "" {
		return ModeCompact, nil
	}
	mode, err := ParseWriteMode(v)
	if err != nil {
		return "", fmt.Errorf("STORAGE_FILE_BUNDLES_WRITE: %w", err)
	}
	return mode, nil
}

// DeduplicateFiles returns the canonical bundle order for one torrent's
// files: stable-sorted by Index, deduplicated by Path keeping the first,
// Extension recomputed from Path, finally sorted by Index then Path.
func DeduplicateFiles(files []model.TorrentFile) []model.TorrentFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]model.TorrentFile, len(files))
	copy(out, files)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	seen := make(map[string]struct{}, len(out))
	kept := out[:0]
	for _, f := range out {
		if _, dup := seen[f.Path]; dup {
			continue
		}
		seen[f.Path] = struct{}{}
		f.Extension = model.FileExtensionFromPath(f.Path)
		kept = append(kept, f)
	}
	// Clear the tail copy so dropped duplicates do not linger in the backing array.
	out = kept
	sort.Slice(out, func(i, j int) bool {
		if out[i].Index != out[j].Index {
			return out[i].Index < out[j].Index
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Querier is the minimal pgx surface used by this package; both
// *pgxpool.Pool and pgx.Tx satisfy it.
type Querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func sortedHashes[V any](m map[protocol.InfoHash]V) []protocol.InfoHash {
	hashes := make([]protocol.InfoHash, 0, len(m))
	for h := range m {
		hashes = append(hashes, h)
	}
	sort.Slice(hashes, func(i, j int) bool {
		return bytes.Compare(hashes[i].Bytes(), hashes[j].Bytes()) < 0
	})
	return hashes
}

func bundleArrays(files []model.TorrentFile) (indexes []int32, paths []string, sizes []int64, extensions []*string) {
	indexes = make([]int32, len(files))
	paths = make([]string, len(files))
	sizes = make([]int64, len(files))
	extensions = make([]*string, len(files))
	for i, f := range files {
		indexes[i] = int32(f.Index) //nolint:gosec // file indexes fit int32 by schema
		paths[i] = f.Path
		sizes[i] = int64(f.Size) //nolint:gosec // file sizes fit int64 by schema
		if f.Extension.Valid {
			ext := f.Extension.String
			extensions[i] = &ext
		}
	}
	return indexes, paths, sizes, extensions
}

// bundleInsertChunkSize caps rows per multi-row INSERT so parameter count
// (5 * bundleInsertChunkSize = 1250) stays well below PostgreSQL's 65535 limit
// while cutting round-trips by up to 250x for batch imports.
const bundleInsertChunkSize = 250

type preparedBundle struct {
	hash       protocol.InfoHash
	indexes    []int32
	paths      []string
	sizes      []int64
	extensions []*string
}

func prepareBundles(filesByHash map[protocol.InfoHash][]model.TorrentFile) []preparedBundle {
	hashes := sortedHashes(filesByHash)
	prepared := make([]preparedBundle, 0, len(hashes))
	for _, hash := range hashes {
		files := DeduplicateFiles(filesByHash[hash])
		if len(files) == 0 {
			continue
		}
		indexes, paths, sizes, extensions := bundleArrays(files)
		prepared = append(prepared, preparedBundle{
			hash:       hash,
			indexes:    indexes,
			paths:      paths,
			sizes:      sizes,
			extensions: extensions,
		})
	}
	return prepared
}

func buildBulkBundleUpsert(chunk []preparedBundle) (string, []any) {
	var sb strings.Builder
	sb.WriteString("INSERT INTO torrent_file_bundles(info_hash,indexes,paths,sizes,extensions) VALUES")
	args := make([]any, 0, len(chunk)*5)
	for i, b := range chunk {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*5 + 1
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d)", base, base+1, base+2, base+3, base+4)
		args = append(args, b.hash.Bytes(), b.indexes, b.paths, b.sizes, b.extensions)
	}
	sb.WriteString(" ON CONFLICT(info_hash) DO NOTHING")
	return sb.String(), args
}

// UpsertBundles writes one bundle row per non-empty hash in multi-row chunks,
// without overwriting existing bundles.
func UpsertBundles(ctx context.Context, db Querier, filesByHash map[protocol.InfoHash][]model.TorrentFile) error {
	prepared := prepareBundles(filesByHash)
	for start := 0; start < len(prepared); start += bundleInsertChunkSize {
		end := start + bundleInsertChunkSize
		if end > len(prepared) {
			end = len(prepared)
		}
		chunk := prepared[start:end]
		querySQL, args := buildBulkBundleUpsert(chunk)
		if _, err := db.Exec(ctx, querySQL, args...); err != nil {
			return fmt.Errorf("filebundles: upsert %d bundles (%s..%s): %w", len(chunk), chunk[0].hash, chunk[len(chunk)-1].hash, err)
		}
	}
	return nil
}

// PersistTorrentFiles writes files inside a GORM transaction per mode.
func PersistTorrentFiles(ctx context.Context, tx *gorm.DB, mode WriteMode, files []*model.TorrentFile) error {
	if len(files) == 0 {
		return nil
	}
	if mode != ModeLegacy && mode != ModeDual && mode != ModeCompact {
		return fmt.Errorf("filebundles: invalid write mode %q", mode)
	}
	tx = tx.WithContext(ctx)
	if mode == ModeLegacy || mode == ModeDual {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(files, 100).Error; err != nil {
			return fmt.Errorf("filebundles: persist legacy files: %w", err)
		}
	}
	if mode == ModeCompact || mode == ModeDual {
		byHash := make(map[protocol.InfoHash][]model.TorrentFile)
		for _, f := range files {
			if f == nil {
				continue
			}
			byHash[f.InfoHash] = append(byHash[f.InfoHash], *f)
		}
		prepared := prepareBundles(byHash)
		if len(prepared) == 0 {
			return nil
		}
		// GORM expands slice arguments in raw SQL, so array binds go through
		// the statement's database/sql connection directly. The statement pool
		// is the live transaction; db.ConnPool stays the root pool.
		pool := tx.ConnPool
		if tx.Statement != nil && tx.Statement.ConnPool != nil {
			pool = tx.Statement.ConnPool
		}
		execer, ok := pool.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		if !ok {
			return fmt.Errorf("filebundles: gorm connection pool cannot bind arrays")
		}
		for start := 0; start < len(prepared); start += bundleInsertChunkSize {
			end := start + bundleInsertChunkSize
			if end > len(prepared) {
				end = len(prepared)
			}
			chunk := prepared[start:end]
			querySQL, args := buildBulkBundleUpsert(chunk)
			if _, err := execer.ExecContext(ctx, querySQL, args...); err != nil {
				return fmt.Errorf("filebundles: upsert %d bundles (%s..%s): %w", len(chunk), chunk[0].hash, chunk[len(chunk)-1].hash, err)
			}
		}
	}
	return nil
}

// PersistTorrentFilesPgx writes files inside a pgx transaction per mode.
func PersistTorrentFilesPgx(ctx context.Context, tx pgx.Tx, mode WriteMode, files []model.TorrentFile) error {
	if len(files) == 0 {
		return nil
	}
	if mode != ModeLegacy && mode != ModeDual && mode != ModeCompact {
		return fmt.Errorf("filebundles: invalid write mode %q", mode)
	}
	byHash := make(map[protocol.InfoHash][]model.TorrentFile)
	for _, f := range files {
		byHash[f.InfoHash] = append(byHash[f.InfoHash], f)
	}
	if mode == ModeLegacy || mode == ModeDual {
		for _, hash := range sortedHashes(byHash) {
			for _, f := range DeduplicateFiles(byHash[hash]) {
				if _, err := tx.Exec(ctx,
					`INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,$2,$3,$4,clock_timestamp(),clock_timestamp()) ON CONFLICT DO NOTHING`,
					hash.Bytes(), int32(f.Index), f.Path, int64(f.Size)); err != nil { //nolint:gosec // schema bounds both values
					return fmt.Errorf("filebundles: persist legacy file %s %q: %w", hash, f.Path, err)
				}
			}
		}
	}
	if mode == ModeCompact || mode == ModeDual {
		if err := UpsertBundles(ctx, tx, byHash); err != nil {
			return err
		}
	}
	return nil
}

// LoadFilesByInfoHashes loads effective files for all hashes in one query.
func LoadFilesByInfoHashes(ctx context.Context, db Querier, hashes []protocol.InfoHash) (map[protocol.InfoHash][]model.TorrentFile, error) {
	out := make(map[protocol.InfoHash][]model.TorrentFile)
	if len(hashes) == 0 {
		return out, nil
	}
	hashBytes := make([][]byte, len(hashes))
	for i, h := range hashes {
		hashBytes[i] = h.Bytes()
	}
	rows, err := db.Query(ctx,
		`SELECT info_hash,"index",path,size,extension,created_at,updated_at FROM torrent_files_effective WHERE info_hash=ANY($1) ORDER BY info_hash,"index",path`,
		hashBytes)
	if err != nil {
		return nil, fmt.Errorf("filebundles: load files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			rawHash []byte
			idx     int32
			path    string
			size    int64
			ext     *string
			created time.Time
			updated time.Time
		)
		if err := rows.Scan(&rawHash, &idx, &path, &size, &ext, &created, &updated); err != nil {
			return nil, fmt.Errorf("filebundles: scan file: %w", err)
		}
		hash, err := protocol.NewInfoHash(rawHash)
		if err != nil {
			return nil, fmt.Errorf("filebundles: file info hash: %w", err)
		}
		f := model.TorrentFile{
			InfoHash:  hash,
			Index:     uint(idx), //nolint:gosec // non-negative by construction
			Path:      path,
			Size:      uint(size), //nolint:gosec // non-negative by construction
			CreatedAt: created,
			UpdatedAt: updated,
		}
		if ext != nil {
			f.Extension = model.NewNullString(*ext)
		}
		out[hash] = append(out[hash], f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filebundles: load files: %w", err)
	}
	return out, nil
}

// LoadFilesByInfoHashesGORM loads effective files for all hashes via GORM.
func LoadFilesByInfoHashesGORM(ctx context.Context, db *gorm.DB, hashes []protocol.InfoHash) (map[protocol.InfoHash][]model.TorrentFile, error) {
	out := make(map[protocol.InfoHash][]model.TorrentFile)
	if len(hashes) == 0 {
		return out, nil
	}
	hashBytes := make([][]byte, len(hashes))
	for i, h := range hashes {
		hashBytes[i] = h.Bytes()
	}
	var files []model.TorrentFile
	if err := db.WithContext(ctx).Table("torrent_files_effective").
		Where("info_hash IN ?", hashBytes).
		Order(`info_hash, "index", path`).
		Find(&files).Error; err != nil {
		return nil, fmt.Errorf("filebundles: load files: %w", err)
	}
	for _, f := range files {
		out[f.InfoHash] = append(out[f.InfoHash], f)
	}
	return out, nil
}

// FillTorrentFilesGORM populates each torrent's Files from the effective
// view, sorted by Path like model.Torrent.AfterFind sorts preloaded files.
func FillTorrentFilesGORM(ctx context.Context, db *gorm.DB, torrents []*model.Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	hashes := make([]protocol.InfoHash, 0, len(torrents))
	seen := make(map[protocol.InfoHash]struct{}, len(torrents))
	for _, t := range torrents {
		if t == nil {
			continue
		}
		if _, ok := seen[t.InfoHash]; ok {
			continue
		}
		seen[t.InfoHash] = struct{}{}
		hashes = append(hashes, t.InfoHash)
	}
	if len(hashes) == 0 {
		return nil
	}
	byHash, err := LoadFilesByInfoHashesGORM(ctx, db, hashes)
	if err != nil {
		return err
	}
	for _, t := range torrents {
		if t == nil {
			continue
		}
		files, ok := byHash[t.InfoHash]
		if !ok || len(files) == 0 {
			continue
		}
		sorted := make([]model.TorrentFile, len(files))
		copy(sorted, files)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
		t.Files = sorted
	}
	return nil
}
