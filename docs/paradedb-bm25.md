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

- Serving-policy semi/anti-joins and SQL JOINs degrade TopK to a normal
  ParadeDB scan plus sort; results are unchanged, only the top-K fast path is
  lost.
- Counts use ParadeDB Aggregate Scan.

Run `dh-waypoint search bm25 verify` after enable and after any ParadeDB
upgrade. A verify failure means the expected `pg_search` plan shape was not
produced (for example a missing index, a version mismatch, or
`SEARCH_BM25_MODE=off`); treat eligible free-text search as degraded to the
stock `tsquery` path and investigate before relying on BM25 ranking.

## Migrating from stock `postgres:16-alpine`

The data directory cannot be reused: musl to glibc plus major version means
collation and on-disk incompatibility. Migrate by dump and restore:

```bash
# 1. Dump from the running PG16 container (synthetic path shown).
docker compose exec postgres pg_dump -U bitmagnet -Fc bitmagnet > backup.dump

# 2. Stop the stock stack, then start ParadeDB on its fresh volume.
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public down
docker compose -f examples/docker-compose.public.yml \
  -f examples/docker-compose.paradedb.yml \
  --env-file examples/.env.public up -d postgres

# 3. Restore into the fresh PG18 volume (reindex after restore is handled by restore).
cat backup.dump | docker compose exec -T postgres pg_restore -U bitmagnet -d bitmagnet --no-owner

# 4. Start the app (migrations run automatically on connect), then enable.
docker compose -f examples/docker-compose.public.yml \
  -f examples/docker-compose.paradedb.yml \
  --env-file examples/.env.public up -d
docker compose exec dh-waypoint dh-waypoint search bm25 enable
docker compose exec dh-waypoint dh-waypoint search bm25 verify
```

For SQL-format dumps, restore with `psql` instead of `pg_restore`. The stock
`dh-waypoint-postgres` volume is never touched by the overlay, so it remains
your fallback copy.

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
