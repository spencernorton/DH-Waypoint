# ParadeDB BM25 Search (Opt-In)

## What it is

An opt-in ParadeDB `pg_search` 0.26.6 BM25 execution path for `torrent_contents`
free-text AND queries. Stock `postgres:16` and `postgres:18` remain fully
supported and are the default. No Go dependencies are added; the Go code stays
MIT and talks plain SQL. ParadeDB itself is AGPL-3.0 and runs as the separate
database server process the operator chooses. Operators should review AGPL
obligations for their deployment; this guide gives no legal advice beyond that.

Pinned image: `paradedb/paradedb:0.26.6-pg18` (PostgreSQL 18). Never use a
`latest` tag; `pg_search` syntax and plan shapes are version-specific.

## How it works

- **Migration 00080** adds only two `IMMUTABLE` helper functions with zero
  storage on stock installs:
  - `torrent_contents_tsv_text(tsvector)` — renders a `tsvector` as its
    space-separated lexemes.
  - `torrent_contents_ep_tokens(jsonb)` — renders the season/episode JSON as
    `s<season>all` / `s<season>e<episode>` tokens.
- **`dh-waypoint search bm25 enable`** (ParadeDB only) creates the `pg_search`
  extension, adds `STORED` generated columns, and builds the index:
  - `search_text` — exact `tsvector` lexemes from `torrent_contents_tsv_text`.
  - `ep_text` — `s<season>all` / `s<season>e<episode>` tokens from
    `torrent_contents_ep_tokens`.

  ```sql
  CREATE INDEX torrent_contents_bm25_idx ON torrent_contents
  USING bm25 (
    id,
    (search_text::pdb.whitespace('lowercase=false')),
    (ep_text::pdb.whitespace),
    (content_type::pdb.literal),
    (video_resolution::pdb.literal),
    seeders,
    published_at
  );
  ```

- **Query routing.** Eligible queries — a pure AND of single-word lexemes —
  run `search_text &&& ?` ordered by `pdb.score(torrent_contents.id)`. All
  other query shapes (phrases, OR, NOT, wildcards, bare-season tokens such as
  `s5`) keep the existing `tsv @@ tsquery` path unchanged.
- **`SEARCH_BM25_MODE=auto|on|off`** (default `auto`). `auto` and `on` both
  use BM25 for eligible queries when the extension, generated column, and
  valid index are detected (`on` is the explicit opt-in spelling); `off`
  always uses `tsquery`. When detection fails, eligible queries fall back to
  `tsquery` in every mode, so a mode value alone can never break search.

## Enable, verify, disable, status

```bash
# 1. Start the stack with the ParadeDB overlay (fresh volume or restored dump).
docker compose -f examples/docker-compose.public.yml \
  -f examples/docker-compose.paradedb.yml \
  --env-file examples/.env.public up -d

# 2. Application migrations (including the migration 00080 helpers) run
#    automatically when the app or the search bm25 command connects.

# 3. Enable BM25 (extension, generated columns, index).
docker compose exec dh-waypoint dh-waypoint search bm25 enable

# 4. Check state and prove the ParadeDB plan shape (see verification below).
docker compose exec dh-waypoint dh-waypoint search bm25 status
docker compose exec dh-waypoint dh-waypoint search bm25 verify

# 5. Disable (drops index and generated columns; safe to re-enable later).
docker compose exec dh-waypoint dh-waypoint search bm25 disable
```

Outside Docker, run the same `dh-waypoint search bm25 <subcommand>` against
your configured database. `enable` refuses to run when `pg_search` is not
available, so a stock PostgreSQL install is never half-converted.

Restart dh-waypoint after `enable` and after `disable`. Capability detection
is cached per database connection pool, and adding or dropping the generated
columns changes the `torrent_contents` row shape, so already-running
processes must recycle their connections before they can use (or stop using)
BM25 cleanly.

## Measured verification

Verified on `paradedb/paradedb:0.26.6-pg18` (PostgreSQL 18.6) with
`EXPLAIN (COSTS OFF)`. An eligible free-text AND query with a limit shows:

```
Limit
  ->  Custom Scan (ParadeDB Base Scan) on torrent_contents
        Exec Method: TopKScanExecState
        TopK Order By: pdb.score() desc
```

The same TopK shape holds when `seeders`, `published_at`, and `content_type`
filters are pushed into the Tantivy query. Note the boundaries:

- **Serving-policy and join Top-K prefix**: Ungrouped BM25 relevance searches
  carrying serving-policy guards (`ExcludeAdult`, required publication review,
  payload evidence, exclusions) or at-most-one joins (`torrents`, `content`,
  `metadata_sources`) automatically execute a two-stage `REPEATABLE READ`
  candidate prefix: Step 1 pulls a bounded top-K `(id, pdb.score(id))` slice
  directly from single-table `torrent_contents` using `TopKScanExecState`, and
  Step 2 joins those IDs into the full policy-guarded query without repeating
  `pdb.score()`. Boundary ties and short pages expand cleanly before falling
  back to the full scan.
- **Episode token pushdown**: Specific episode filters (`TorrentContentEpisodesCriteria`
  with explicit episode numbers) push `ep_text ||| 's<S>all s<S>e<E>'` (or the
  multi-episode conjunction) straight into the Tantivy boolean query while
  preserving whole-season-pack semantics.
- **Counts and observability**: Counts use ParadeDB Aggregate Scan. Prometheus
  exports `dh_waypoint_search_queries_total{engine="bm25|tsquery",path="prefix|direct"}`,
  `dh_waypoint_postgres_bm25_index_valid`, `dh_waypoint_postgres_bm25_index_size_bytes`,
  and `dh_waypoint_postgres_file_bundles_backfill_done`.

Run `dh-waypoint search bm25 verify` after enable and after any ParadeDB
upgrade. A verify failure means the expected `pg_search` plan shape was not
produced (for example a missing index, a version mismatch, or
`SEARCH_BM25_MODE=off`); treat eligible free-text search as degraded to the
stock `tsquery` path and investigate before relying on BM25 ranking.

## Migrating from stock `postgres:16-alpine`

The data directory cannot be reused: musl to glibc plus major version means
collation and on-disk incompatibility, and PostgreSQL 18 containers require
mounting the data volume at `/var/lib/postgresql` rather than
`/var/lib/postgresql/data`. Migrate by logical custom-format dump and parallel
restore so every B-tree, GIN, and trigram index is rebuilt cleanly under PG 18
glibc collations.

**Recommended pre-step (schema 81+):** run `dh-waypoint storage compact-files --prune-legacy`
followed by `VACUUM FULL torrent_files; ANALYZE;` on PG 16 before cutover. In
rehearsals this shrank file storage by 86% and total database size by 40–47%,
cutting dump size and restore time nearly in half with zero publication
revision drift.

### Automated helper (`scripts/migrate-to-paradedb.sh`)

`scripts/migrate-to-paradedb.sh` automates pre-flight checks, custom-format
`pg_dump` (`-Fc -Z 1`), parallel `pg_restore`, `ANALYZE`, exact row-count plus
publication-revision / payload-approval parity verification across all core
tables, optional `dh-waypoint search bm25 enable` + `verify --verbose`, and an
optional machine-readable JSON timing report:

```bash
# 1. Pre-flight check only (verifies versions, pg_search preload, target emptiness).
scripts/migrate-to-paradedb.sh \
  --source-dsn "postgres://bitmagnet:<pass>@127.0.0.1:5432/bitmagnet?sslmode=disable" \
  --target-dsn "postgres://bitmagnet:<pass>@127.0.0.1:5433/bitmagnet?sslmode=disable" \
  --pg-container dh-waypoint-paradedb \
  --dry-run

# 2. Execute cutover (run after stopping writers to the source PG16 database).
scripts/migrate-to-paradedb.sh \
  --source-dsn "postgres://bitmagnet:<pass>@pg16-host:5432/bitmagnet?sslmode=disable" \
  --target-dsn "postgres://bitmagnet:<pass>@127.0.0.1:5432/bitmagnet?sslmode=disable" \
  --bm25-dsn "postgres://bitmagnet:<pass>@127.0.0.1:5433/bitmagnet?sslmode=disable" \
  --pg-container dh-waypoint-paradedb \
  --jobs 4 \
  --compress 1 \
  --dh-waypoint-bin /usr/local/bin/dh-waypoint \
  --report-json /tmp/paradedb-migration-report.json
```

Pass `--skip-bm25` to end the offline maintenance window immediately after
parity verification, then run `dh-waypoint search bm25 enable` and
`dh-waypoint search bm25 verify --verbose` separately.

### Manual Docker Compose flow

```bash
# 1. Dump from the running PG16 container.
docker compose exec postgres pg_dump -U bitmagnet -Fc -Z 1 --no-owner --no-privileges bitmagnet > backup.dump

# 2. Stop the stock stack, then start ParadeDB on its fresh /var/lib/postgresql volume.
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public down
docker compose -f examples/docker-compose.public.yml \
  -f examples/docker-compose.paradedb.yml \
  --env-file examples/.env.public up -d postgres

# 3. Parallel restore into the fresh PG18 volume (indexes are rebuilt cleanly).
docker compose cp backup.dump postgres:/tmp/backup.dump
docker compose exec postgres pg_restore -U bitmagnet -d bitmagnet -j 4 --no-owner --no-privileges --exit-on-error /tmp/backup.dump
docker compose exec postgres rm -f /tmp/backup.dump
docker compose exec postgres psql -U bitmagnet -d bitmagnet -c "ANALYZE;"

# 4. Start the app (migrations run automatically on connect), then enable and verify.
docker compose -f examples/docker-compose.public.yml \
  -f examples/docker-compose.paradedb.yml \
  --env-file examples/.env.public up -d
docker compose exec dh-waypoint dh-waypoint search bm25 enable
docker compose exec dh-waypoint dh-waypoint search bm25 verify --verbose
```

For SQL-format dumps, restore with `psql` instead of `pg_restore`. The stock
`dh-waypoint-postgres` volume is never touched by the overlay, so it remains
your fallback copy.

### Measured upgrade rehearsals (PG 16.15 Alpine -> ParadeDB 0.26.6 / PG 18.6)

Rehearsed end-to-end with `scripts/migrate-to-paradedb.sh --jobs 4 --compress 1`
on NVMe storage (`shared_buffers=2GB`, `maintenance_work_mem=1GB`) at two
compacted schema-81 catalog sizes:

- **129,019 torrents / 115,989 `torrent_contents` / 1,566,191 legacy files (81,812 compacted bundles):**
  - Pre-cutover compaction + `VACUUM FULL torrent_files`: 11s, file storage `528 MB -> 73 MB` (-86.2%), total DB `1,134 MB -> 681 MB` (-39.9%), 0 publication-revision bumps.
  - Logical dump: `3.80s` (`175.6 MiB`); parallel restore (`-j 4`): `6.01s`; `ANALYZE`: `1.90s`; 12-table + revision/approval parity: `3.20s` (100% match).
  - `bm25 enable`: `8.93s` (`52.2 MiB` BM25 index, 0 invalid indexes); `bm25 verify --verbose`: `0.07s` (4/4 `TopKScanExecState`).
  - Final PG 18 DB with BM25: `771.6 MiB` (still 32.0% smaller than uncompacted PG 16); peak container RAM ~`1.2 GiB` each.
- **343,514 torrents / 308,360 `torrent_contents` / 4,187,920 legacy files (218,024 compacted bundles):**
  - Pre-cutover compaction + `VACUUM FULL torrent_files`: 36s, file storage `1,413 MiB -> 196 MiB` (-86.1%), total DB `2,581 MiB -> 1,365 MiB` (-47.1%), 0 publication-revision bumps.
  - Logical dump: `7.37s` (`336.8 MiB`); parallel restore (`-j 4`): `11.30s`; `ANALYZE`: `2.59s`; 12-table + revision/approval parity (`sum(revision)` and 406 approved reviews): `3.54s` (100% match).
  - `bm25 enable`: `20.70s` (`119.1 MiB` BM25 index, ~14,900 docs/sec, 0 invalid indexes); `bm25 verify --verbose`: `0.07s` (4/4 `TopKScanExecState`).
  - Final PG 18 DB with BM25: `1,538 MiB` (40.4% smaller than uncompacted PG 16); total data cutover window without BM25 build: `24.8s` (`45.6s` including BM25 build).

## Costs and rollback

- **Storage.** Enabling adds the `search_text` and `ep_text` generated columns
  plus `torrent_contents_bm25_idx` on top of the existing `tsvector` and GIN
  index. Budget extra disk roughly proportional to the indexed text volume.
- **Enable cost.** Adding `STORED` generated columns and building the BM25
  index take a table rewrite and locks on `torrent_contents`. Expect a
  maintenance window on large tables; run enable outside peak crawl ingest.
- **Disable.** `dh-waypoint search bm25 disable` drops the index and the
  generated columns. The `pg_search` extension and the migration 00080 helper
  functions remain; both are harmless and cost no storage.
- **Full rollback to stock.** Stop the stack, remove the overlay, and restore
  the stock volume or dump. Going back from a live PG18 ParadeDB volume to
  stock `postgres:16-alpine` requires a dump/restore back; the PG18 glibc data
  directory is not readable by PG16 Alpine.
- **Approval snapshots.** `search_text` and `ep_text` are excluded from
  publication source snapshots, so enabling never invalidates processor
  approvals.
