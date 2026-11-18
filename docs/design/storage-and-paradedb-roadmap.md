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

## Phase 2: 1-Row Compact File Array (`torrents.files_compact`)

In large DHT catalogues, `torrent_files` accounts for roughly half of total database disk usage because PostgreSQL stores a 23-byte heap tuple header plus primary-key and foreign-key B-tree entries per child file row.

The staged migration path consolidates per-torrent file metadata into a single compressed column on `torrents`:

- Add `torrents.files_compact bytea` (zstd-compressed canonical JSON array of `[index, path, size]` tuples, utilizing PostgreSQL TOAST compression for large multi-file releases).
- Dual-read in `internal/processors/source.go`, `internal/database/search/hydrate_torrent_files.go`, and GraphQL/REST file resolvers (`COALESCE` from `files_compact`, falling back to `torrent_files` during online background backfill).
- Backfill `files_compact` in bounded keyset batches (`10,000` infohashes per transaction) with zero lock contention, then drop `torrent_files` once `100%` of `multi` rows carry `files_compact`.

## Opt-In ParadeDB `pg_search` BM25 Integration

`DH-Waypoint` remains 100% MIT-licensed in Go and communicates with ParadeDB solely through standard SQL queries when detected by `search.DetectDBCapabilities`:

- Stock `postgres:16` / `postgres:18` containers require only `pg_trgm` and `btree_gin` (unchanged).
- Operators who swap the PostgreSQL image to `paradedb/paradedb:latest-pg18` and create `torrent_contents_bm25_idx` unlock native Block-Max WAND BM25 top-K scoring (`paradedb.score(id)`) with transactional MVCC visibility and zero dual-write sync lag.
