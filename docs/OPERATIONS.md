# Backend deployment and operations

DH-Waypoint 5.x is a Go backend with PostgreSQL storage. It exposes machine APIs
and metrics, and runs independently of any browser interface or hosted account
system. Optional metadata, evidence, processor and cleanup features use configuration
you supply; installation does not establish that those features are active.

## Install the public stack

Use Docker Compose with persistent storage and outbound TCP/UDP access:

```sh
git clone https://github.com/spencernorton/DH-Waypoint.git
cd DH-Waypoint
cp examples/.env.example examples/.env.public
openssl rand -hex 32   # POSTGRES_PASSWORD
openssl rand -hex 32   # TORZNAB_API_KEY
```

Save the generated values in `examples/.env.public`, then run:

```sh
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public config --quiet
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public ps
curl --fail http://localhost:3333/metrics
```

With `HTTP_SERVER_API_KEY` set, add `-H "X-API-Key: <key>"` to that request.

### Import guard holds

An optional `PROCESSORS_IMPORT_HOLD_TOKEN` authorizes a download import guard to
call `POST /api/publication/v1/import-hold/<info-hash>` with that bearer token.
Send a JSON `request_id` (64 lowercase hexadecimal digits) and a `reason`
of `unsafe_media_extension` or `unsafe_file_attributes`. Use a stable request
ID for retries of the same event. The endpoint applies a publication hold to
an existing public torrent and retains an existing operator hold. It is
disabled by default and has no authority to remove holds or administer
processors.

The example publishes backend HTTP (port 3333) on `127.0.0.1` only, unless
`DH_WAYPOINT_HTTP_BIND` names another address, and
peer traffic on the configured peer port. PostgreSQL stays on the Compose network. There is no browser page.
Use [quickstart](quickstart.md) and [examples](../examples/README.md) for the
exact network layout and commands.

## Configure and verify integrations

| Area | Verify |
|---|---|
| PostgreSQL | Password, persistent volume, capacity and tested restore |
| DHT | UDP routing, bootstrap peers and persisted-torrent metrics |
| Torznab | API key and capabilities from the client network |
| GraphQL/import/metrics | Trusted network or authenticated reverse proxy |
| Metadata | Provider configuration, quota and actual provider responses |
| Evidence | Configured source endpoints and independent webhook secret |
| Processors/cleanup | Explicit mode, bounded resources, status metrics and recovery plan |

[Configuration](configuration.md) documents backend settings. Client services
on another host cannot use your own `localhost`; choose an address reachable
from their network and set `DH_WAYPOINT_HTTP_BIND` to publish the port there. Set the API keys
before any host you do not trust can reach port 3333.

## Health and capacity

Verify both services are healthy. Observe `/metrics`, container logs, queue
depth, database growth and provider errors. Crawl warmup varies with routing
and network conditions; an empty early search is not a fixed-duration failure.
Test Torznab capabilities and a synthetic request before connecting a full
client stack. Evaluate optional processing in shadow/dry-run mode before
accepting live verdicts or deletion.

## Catalogue removal

Catalogue removal and crawler blocking require the complete, bounded recovery
transition. The application does not attach this disabled foundation: legacy
CEL delete actions, enforced content-filter drops, and GraphQL deletion
therefore return a recovery-disabled error before deletion or bloom admission.
Ordinary matching and keep/review rows still persist, including independent
rows in a batch whose removal is held.

Retention purge remains held. Embedded synchronous and Batch junk inference have been retired:
their previous partial snapshots cannot restore every cascading source row.
Dry-run observations and retained provider results,
existing quarantine expiry and operator restoration remain available. A held
destructive Batch run retains its successful provider receipts in `finalizing`;
it is not recorded as applied and does not dispatch again for that hold.

Legacy archive hard purge is held as well; the API returns conflict without
deleting its snapshot or declaring the torrent dead. Legacy restore validates
captured hashes and source bounds, rechecks current raw state and protected
authority, and refuses competing private/canonical/verdict facts. It preserves
newer source observations and retains permanent processing/dispatch histories.
Old partial snapshots cannot reconstruct facts that were never captured.

The explicit `blocking.WithRecovery` integration is for callers that have
qualified source protection, complete capture, bounded retention/storage and
restoration. It does not authorize a deletion policy or configure the public
application. Never enable legacy title/origin/zero-seed decisions merely because
capture is available. Do not downgrade to an older unguarded binary to bypass
the hold.

## Quarantine maintenance

Quarantine expiry retains snapshots indefinitely; it does not delete them or
blacklist a hash. Maintenance processes oldest eligible snapshots in chunks of
at most 256, with at most 1,024 transitions before candidate work in a cycle.
The existing unexpired-snapshot index keeps the retained archive out of that
scan. A larger backlog remains eligible for later ordinary cycles.

When the verdict ledger is configured, each expiry chunk commits its marker,
current state and events together. A failed or canceled chunk rolls back and
can be retried normally; committed chunks are not repeated. Restore and explicit
deletion also commit their ledger transition with the snapshot operation.
Ledger failures therefore fail those operator actions while retaining their
original snapshot, rather than report success with inconsistent state.

Restore validates the locked snapshot after acquiring the raw-torrent lock.
If another operation refreshed or removed that snapshot, the restore rolls
back; an operator can review the current version and retry. Delayed quarantine
observations cannot change a completed restore, deletion or expiry back to
quarantined.

## Processor migration

For mandatory positive review and optional piece-bound format evidence, see
[required publication review](processors/publication.md). Publication holds
and evidence are independent of metadata matching and retention cleanup.

Read [the v5 migration and rollback procedure](processors/migration.md) before upgrading. Stop retired workers, preserve database/evidence backups and reconcile outstanding provider jobs without redispatch. No plugin is required for startup.

## Backup, upgrade and recovery

Back up PostgreSQL and optional configuration/data volumes. Store secrets
separately in encrypted backups. Restore into an isolated database with DHT
and external calls disabled until recovered configuration has been reviewed.

Record the running release and immutable image digest. Upgrade with the
published digest and verify migrations, APIs, metrics and crawl progress.
A migration can make a binary-only downgrade unsafe; retain the matching
backup and previous artifact. For 3.x combined images, follow
[the 4.0 migration](migration-v4.md).

### Optional database migration to ParadeDB (PostgreSQL 18 + BM25)

Upgrading DH-Waypoint on an existing `postgres:16-alpine` or stock `postgres:18` database requires **no database migration**: Goose migrations run in place on connect and search remains on stock `tsquery`.

Switching PostgreSQL to `paradedb/paradedb:0.26.6-pg18` is **100% opt-in**:

- **Why migrate:** unlocks native ParadeDB BM25 Top-K ranking (`TopKScanExecState`), TV episode token pushdown (`ep_text`), and full table/index bloat reclamation after online `torrent_file_bundles` compaction (shrinking a compacted 15.8M-torrent catalogue from `100.0 GiB` on PG 16 to `66.7 GiB` on PG 18 *including* the `3.93 GiB` BM25 index). Because PostgreSQL 16 Alpine (`musl`) and PostgreSQL 18 Debian (`glibc`) differ in both major on-disk format and libc collations — and PG 18 mounts its data volume at `/var/lib/postgresql` rather than `/var/lib/postgresql/data` — migrating requires a logical dump and parallel restore into a fresh volume.
- **What the process looks like:**
  1. While live (zero downtime), run `dh-waypoint storage compact-files --prune-legacy` to compact multi-file `torrent_files` rows into `torrent_file_bundles`.
  2. Stop writers briefly during the maintenance window.
  3. Run `scripts/migrate-to-paradedb.sh` (pass `--format directory --jobs 8` for multi-million-row databases) into a fresh PG 18 `/var/lib/postgresql` volume; the script checks 13-table exact row counts and publication/approval invariants before activation and warns if extra cluster roles need `pg_dumpall --roles-only` + ACL replay.
  4. Run `dh-waypoint search bm25 enable` and `dh-waypoint search bm25 verify --verbose`, then start DH-Waypoint with the `examples/docker-compose.paradedb.yml` overlay.
  5. Keep the original PG 16 volume untouched until soak verification completes so rollback is an immediate Compose switch.
- **How long it takes (measured on NVMe):**
  - ~129K torrents: **~24 seconds** total (dump, restore, parity, and BM25 build).
  - ~344K torrents: **~46 seconds** total (~25s data cutover + ~21s BM25 build).
  - ~15.8M torrents / 14.2M `torrent_contents` (12-core NVMe, `--format directory --jobs 8`): **~46 minutes** offline data cutover (`10.9 min` dump, `32.3 min` parallel restore, `0.8 min` `ANALYZE`, `2.0 min` exact parity) plus **~37 minutes** for `bm25 enable` (`25.8 min` single-pass `ALTER TABLE`, `11.5 min` concurrent BM25 index build) = **~84 minutes** end-to-end.

See [ParadeDB BM25 Search & Migration](paradedb-bm25.md) for complete commands, verification checks, and rollback steps.

See [security](operations/security.md), [monitoring](operations/monitoring.md)
and [troubleshooting](troubleshooting.md) for further operational guidance.

### Release-level English audio

Set `SERVING_REQUIRE_ENGLISH_MAIN_AUDIO=true` with a pinned required publication
processor to limit consumer results to English-audio films and series.
The processor must supply a completed language proposal with English in its
audio list, a boolean anime decision, a film or series category and a positive
release audio decision. The matching fields in the result trace are
`audio_language`, `anime` and `neural_heads.category`. Anime needs a dub claim
for this release. Conflicting media filenames and partial or uncertain packs
remain held. A source change invalidates the grant through the existing
publication revision. This feature reads torrent metadata and processor
results.

After migrating, start in shadow with the option off. New reviews retain their
audio verdict in `processor_publication_reviews.release_audio_v1`. Reconcile
older reviews in small transactions with
`SELECT * FROM publication_reconcile_audio_v1($1,$2,$3)`, binding the reviewer
ID, pinned artifact digest and a batch size from 1 to 10,000. Each call returns
counts grouped by outcome. Repeated calls skip reviewed rows. A missing or
superseded processor result stays unresolved until a new review.
For missing receipts, budget model executions separately and call
`publication_retry_missing_audio_v1` with the same three arguments. This schedules
specific hashes through the normal processor admission queue when English-audio
publication is active. Repeating a request skips already requested rows. Ambiguous
audio remains parked for better evidence.
Check agreement and counts before enabling the option. Torznab and the
authenticated public-hash check advertise the active policy with
`X-Publication-Audio: reviewed-english-main-audio-v1`.

For offline metadata recovery, run `go run ./scripts/metainfo_inventory DIRECTORY`
against originals named by expected torrent hash. Each JSON line reports the
native parser disposition and size. Public originals with safe file attributes
also carry a SHA-256 of the whole file. A recovery worker can bind a staged
copy to those exact bytes before submitting it to the normal metainfo importer.
