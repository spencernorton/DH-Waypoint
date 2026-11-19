# Compact File Storage and Opt-In ParadeDB BM25 Architecture

## Goals

- Cut PostgreSQL storage overhead on multi-tens-of-millions torrent catalogues without requiring external search daemons or giving up transactional consistency.
- Keep stock `postgres:16` and `postgres:18` deployments fast and zero-dependency out of the box while letting high-scale operators opt into `paradedb/paradedb` PG18 BM25 ranking automatically.

## Phase 1 (Shipped in v5.11)

- **BEP 47 Padding Exclusion**: `model.TorrentFromMetadata` and `tracker.Store` skip BEP 47 padding entries (`attr` containing `p`, `.pad` segments, and `_____padding_file_` placeholders) while preserving each real file's original metainfo index and computing `torrents.size` across real payload files.
- **HOT Swarm Updates**: Dropping the four unread secondary B-tree indexes on `torrents_torrent_sources` and setting `fillfactor = 90` allows tracker/DHT seeder updates to stay Heap-Only Tuple (HOT) on existing 8 KiB pages, cutting write-ahead log (WAL) volume by ~85%.
- **Bounded Relevance & Newest-First Default**:
  - Torznab defaults free-text searches to `published_at DESC` (`TORZNAB_DEFAULT_ORDER=newest`), matching how automation indexers consume feeds and eliminating broad `ts_rank_cd` heap scoring.
  - Explicit relevance queries cap `ts_rank_cd` scoring to the newest 1,000 GIN matches on `torrent_contents`.
  - Unlinked identifier FTS fallback runs inside a `SAVEPOINT` with a configurable `40ms` sub-deadline (`SEARCH_UNLINKED_FALLBACK_TIMEOUT`) and `SET LOCAL max_parallel_workers_per_gather = 0`, returning linked rows cleanly if the unlinked scan times out.
- **Composite Episode GIN Index**: `torrent_contents_episode_keys_idx` indexes `(content_type, content_source, content_id, season/episode)` tokens in a compact partial GIN index for sub-millisecond TV season and episode lookups.

## Phase 2: Compact 1-Row-Per-Torrent File Bundles (`torrent_file_bundles`, implemented)

In large DHT catalogues, `torrent_files` accounts for roughly half of total database disk usage because PostgreSQL stores a heap tuple per file plus the `(info_hash, path)` primary-key and `(info_hash, index)` unique B-tree entries. Migration `00081_torrent_file_bundles.sql` adds a dedicated sidecar table instead of a column on `torrents`: `torrents` receives frequent `ON CONFLICT DO UPDATE` writes on DHT re-discovery while file lists are immutable per infohash, so the sidecar avoids `torrents` heap bloat and write amplification while eliminating the per-file B-tree overhead.

- **Schema**: `torrent_file_bundles (info_hash bytea PRIMARY KEY REFERENCES torrents ON DELETE CASCADE, indexes integer[], paths text[], sizes bigint[], extensions text[], created_at, updated_at)` with an equal-cardinality `CHECK` and `STORAGE EXTENDED` on the text arrays so large bundles TOAST-compress. `NULL` array elements preserve extension-free paths exactly like the legacy generated `extension` column.
- **Dual reads**: `torrent_files_for_hash(bytea)` returns legacy-shaped rows from the bundle when one exists (otherwise from `torrent_files`), and the `torrent_files_effective` view does the same for multi-hash queries. The processors' canonical source snapshot, search hydration, `TorrentFiles` search and facets, the serving adult guard, CSAM sweep/removal, catalogue recovery snapshots, and every other reader use these bundle-first paths, producing byte-for-byte identical snapshots and source digests in all write modes.
- **Write modes**: `STORAGE_FILE_BUNDLES_WRITE` selects `compact` (default: bundle only), `dual` (both representations, for zero-risk rollback windows), or `legacy` (`torrent_files` only, emergency fallback). Writers deduplicate by path keeping the first occurrence in index order, matching the legacy `(info_hash, path)` first-writer-wins key, and bundles are write-once (`ON CONFLICT DO NOTHING`).
- **Online backfill**: `dh-waypoint storage compact-files` walks `torrent_files` in bounded keyset batches (`--batch-size`, default 2000 hashes per transaction; `--pause`, default 25ms; `--max-batches`), with a resumable cursor in `key_values` (`storage_file_bundles_backfill`). Every batch sets `dh_waypoint.suppress_publication_trigger = 'on'`, so migration and `--prune-legacy` deletion cause zero publication-revision increments and invalidate no approvals. `--verify-only` checks exact `(index, path, size, extension)` parity, and `--status` reports legacy/bundle counts, relation sizes, and the saved cursor (`--status --fast` reads O(1) `pg_class.reltuples` estimates without scanning `torrent_files`). Live runs stream NDJSON progress updates to stderr (`--progress-interval`, default `5s`) while keeping stdout pure JSON, checkpoint cleanly after the active batch commits on `SIGINT`/`SIGTERM`, and write new bundles in 250-row bulk `INSERT` chunks. `torrent_files` itself is dropped only in a later migration, after operators finish backfill and parity verification.
- **Measured size**: on a synthetic PostgreSQL 16 batch of 200 torrents with 40 files each, `torrent_files` occupies 3,637,248 bytes (heap plus indexes) versus 327,680 bytes for `torrent_file_bundles` — about 11.1x smaller (`TestBundleStorageSavings` in `internal/database/filebundles`).

## Opt-In ParadeDB `pg_search` BM25 Integration (Shipped)

`DH-Waypoint` remains 100% MIT-licensed in Go and communicates with ParadeDB solely through standard SQL queries when detected by `search.DetectDBCapabilities`. Operator guide: [docs/paradedb-bm25.md](../paradedb-bm25.md).

- Stock `postgres:16` / `postgres:18` containers require only `pg_trgm` and `btree_gin` (unchanged).
- Migration `00080` adds only the `IMMUTABLE` helpers `torrent_contents_tsv_text(tsvector)` and `torrent_contents_ep_tokens(jsonb)` with zero storage on stock installs.
- Operators who swap the PostgreSQL image to the pinned `paradedb/paradedb:0.26.6-pg18` and run `dh-waypoint search bm25 enable` unlock native Block-Max WAND BM25 top-K scoring (`pdb.score(torrent_contents.id)`) with transactional MVCC visibility and zero dual-write sync lag. The `enable` step creates the `pg_search` extension, adds `STORED` generated columns `search_text` and `ep_text`, and builds `torrent_contents_bm25_idx`.
- Eligible queries (pure AND of single-word lexemes) run `search_text &&& ?` ordered by `pdb.score(torrent_contents.id)`; all other shapes (phrases, OR, NOT, wildcards, bare-season tokens) keep the existing `tsv @@ tsquery` fallback. `SEARCH_BM25_MODE=auto|on|off` (default `auto`) controls this routing.
- Lifecycle and plan-shape checks: `dh-waypoint search bm25 enable`, `status`, `verify`, and `disable`. `disable` drops the index and generated columns; the extension and helper functions remain harmless.
