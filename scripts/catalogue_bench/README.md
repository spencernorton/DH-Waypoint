# Disposable catalogue SQL benchmark

This harness measures SQL retrieval and persistence costs. It does not measure
HTTP serialization, DHT network yield, classification accuracy or a complete
production installation. All rows, identifiers, files and proposals are invented.
Do not point it at an application database.

## Reproduce

Use an isolated Linux Docker host with at least 200 GiB free on the database
filesystem. The tested container has six CPUs, 16 GiB RAM, no swap allowance,
2 GiB PostgreSQL shared buffers and durable WAL settings. Run one suite at a time.
The 2.4M, 10M and 32M fixtures grow the same deterministic corpus. 100M is outside
this harness's bounds. Preflight records free bytes and refuses the configured
100 GiB reserve both initially and between load batches. Set `--disk-path` if
the harness and Docker volume reside on different filesystems.

Create a dedicated network, volume and database; these commands use disposable
test credentials. The internal network does not expose a public database port.

```sh
docker network create --internal dh-waypoint-catalogue-bench-test
docker volume create dh-waypoint-catalogue-bench-test
docker run -d --name dh-waypoint-catalogue-bench-test \
  --label purpose=disposable-synthetic-benchmark \
  --network dh-waypoint-catalogue-bench-test \
  --cpus=6 --memory=16g --memory-swap=16g --pids-limit=128 --shm-size=2g \
  -e POSTGRES_PASSWORD=synthetic-benchmark-only \
  -e POSTGRES_DB=dh_waypoint_bench_catalogue \
  -v dh-waypoint-catalogue-bench-test:/var/lib/postgresql/data postgres:16-alpine \
  -c shared_buffers=2GB -c effective_cache_size=8GB \
  -c maintenance_work_mem=512MB -c work_mem=16MB \
  -c max_parallel_workers_per_gather=2 -c max_connections=24 \
  -c track_io_timing=on -c max_wal_size=4GB

python3 -m venv .venv
.venv/bin/pip install -r scripts/catalogue_bench/requirements.txt
BENCH_IP=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' dh-waypoint-catalogue-bench-test)
export CATALOGUE_BENCH_DSN="postgresql://postgres:synthetic-benchmark-only@$BENCH_IP:5432/dh_waypoint_bench_catalogue"
.venv/bin/python scripts/catalogue_bench/monitor.py \
  dh-waypoint-catalogue-bench-test results/resources.jsonl &
MONITOR_PID=$!
.venv/bin/python scripts/catalogue_bench/run_suite.py \
  dh-waypoint-catalogue-bench-test --out results/catalogue \
  --sizes 2400000,10000000,32000000 --samples 30 --cold-samples 10
kill "$MONITOR_PID"
```

The database name must start with `dh_waypoint_bench_`; the seed additionally
requires `--disposable` and the fixture sentinel. The suite validates the
container label and resource caps before stopping, crashing or restarting it.
It never reads credentials or connects to the backend. The manifest records the
actual image ID, PostgreSQL version, settings and harness hashes. Pin the
recorded image digest when reproducing a published run.

Individual phases are available through `bench.py seed`, `indexes`, `run`,
`storage` and `reindex`. `indexes --variant baseline` removes the candidate
indexes. `seed --workers 4` uses four independent transactional loaders. A
failed parallel load can leave noncontiguous committed ranges; the loader
refuses that state rather than silently skipping holes. Recreate that disposable
fixture. A complete prefix can grow to the next scale without reseeding it.
Growth of at least one million records rebuilds the harness's secondary query
indexes after the bulk load; primary, unique and foreign-key constraints remain
active. `--keep-seed-indexes` retains them during construction. The seed policy
and actual build times are recorded. Bulk population throughput is distinct from
the concurrent ingestion phase, which always runs with the tested index set.

After the main suite, `prepared.py --out results/prepared.jsonl --variant candidate`
compares automatic, forced generic and forced custom plans without changing the
corpus, recording the server's generic/custom execution counters. It includes a
combined canonical-ID/episode/quality query and checks its complete expected
page with an independent mathematical oracle. The
`indexes --variant selected` experiment removes the unused raw B-tree and the
episode GIN, replaces the experimental raw TSV GIN with the v2 API's literal
name-substring trigram index, retains external-ID/page indexes, and adds an
ordered canonical-ID index to test skew-sensitive prepared plans. It uses
concurrent index creation. `indexes --variant retained` removes the wider published/typed browse indexes
while keeping ordered canonical lookup, reverse identifiers and migration71's
raw-name trigram operator. It is the final 32M index profile. `--keep-current-indexes`
with `--skip-seed` lets a full suite validate that already established set rather
than rebuild it before timing. The run records its actual inventory and bytes.
The original 25-case mix stays fixed. Request the supplemental v2 profile with
`--cases raw_substring_hot,raw_substring_rare,raw_substring_miss,raw_substring_wildcard,raw_v2_keyset`.
It uses escaped `ILIKE`, canonical-hash ordering and 101 rows for a 100-row page
plus has-next-page. This is separate from the token-based full-text experiment.
It also follows raw serving's direct private-field predicate. The original
25-case model retains a redundant private self-semijoin on raw queries, so its
raw FTS/hash-cursor timings must not be attributed to the v2 API. Both profiles
still use reduced quarantine/adult guards, not the complete application policy.

`resolved.py --out results/resolved.jsonl` measures a second profile after the
retained 32M suite. It resolves at most two complete canonical keys, then reads
the page in one read-only repeatable-read transaction. A singleton adds direct
canonical equality plus the original identifier guard; multiple keys retain the
original semijoin. Timing includes BEGIN, lookup, page and COMMIT round trips.
Automatic/custom/generic probes include hot/rare aliases, a missing alternate,
a duplicate canonical alternative and two distinct keys. Failed warmups and
skipped observations remain visible. These are SQL transactions, not GORM
hydration or HTTP latency. Go integration tests separately cover source wiring,
namespace/OR behavior, policy before paging, alias revocation and cancellation.

Additional cold queries can use `run_suite.py --cold-only --skip-seed` with one
already loaded size, the current `--variants` label, and `--cold-cases` containing
comma-separated case names. This mode leaves indexes untouched and does not
claim a complete scale qualification.

To summarize raw files and render the optional chart:

```sh
python3 scripts/catalogue_bench/summarize.py results/catalogue --out results/summary.json
.venv/bin/pip install -r scripts/catalogue_bench/plot-requirements.txt
.venv/bin/python scripts/catalogue_bench/plot.py results/summary.json --out results/latency.png
.venv/bin/python scripts/catalogue_bench/plot_costs.py results/summary.json --out results/costs.png
python3 scripts/catalogue_bench/resources.py results/summary.json results/resources.jsonl --out results/resource-summary.json
```

## Corpus and authority

Every torrent has a reproducible 20-byte synthetic hash, a raw release name,
timestamps, source facts and file inventory. There are 95% single-file rows,
4% ten-file packs and 1% hundred-file packs: 2.35 files per torrent. Names include
title, season, episode, resolution, release and group tokens. Five percent share
one hot identity; the long tail spans about one million other identities.
These are declared synthetic assumptions, not measurements of a live catalogue.
The scale fixture has one content projection per torrent; a separate PostgreSQL
regression covers content siblings. File-heavy and multi-content catalogues
need their own population qualification.

The same physical corpus supports raw-name FTS and accepted-identity retrieval.
Raw-name queries read `torrents.tsv`, including a regression with the content
projection temporarily absent. The baseline retains its historical B-tree;
the candidate adds GIN. Content search retains its existing GIN separately.
Seventy percent have accepted canonical mappings. Ten percent have stale
source revisions, ten percent have uncertain proposals, and ten percent have
no proposal. Plugin ID, confidence, verification and source revision live in
`annotations`; they do not authorize serving. Catalogue identities and external
ID attributes are separate. Private flags, quarantine and accepted adult facts
are rechecked by each query. This is a reduced model of those serving predicates;
the backend's full snapshot, privacy-label and name-policy guards are separately
tested by the Go integration suite.

Raw text and enrichment-assisted fallback queries must produce the same complete
eligible set. Unknown mappings remain text candidates. The bounded fallback
first applies eligibility, then takes the top 100 of each disjoint branch and
merges the candidates. Its first page must exactly equal the unassisted
content-name text page in both membership and published order. Raw v2 uses a
different hash order and literal substring semantics. A candidate without accepted mapping is not certified
as a canonical identity. An ID-only API request cannot establish the identity of
an unclassified row; it needs a separate text fallback or subsequent enrichment.

## Measurements

- Exact hash, hot/rare canonical IDs, external IDs, common/rare/missing text,
  type and quality browse, seasons/episodes including whole-season packs,
  grouped selection, deep offset, keyset traversal and two fallback strategies.
- Complete `EXCEPT` checks in both directions for equivalent predicates, plus
  page equality for bounded fallback. A separate mathematical generator oracle
  checks returned identity, type, quality, episode and eligibility. Stale or
  uncertain proposals must have zero promoted canonical mappings.
- Warm samples use established connections after an explicit warmup. Four query
  workers run alongside source re-observations, metadata updates and real inserts
  of synthetic torrents, contents, proposals and file inventories. Inserted tail
  rows are removed in FK order after that phase to restore the initial corpus for
  the next variant; committed update churn remains. Raw counters distinguish
  inserted torrents from updated records.
- Cold trials stop only the labeled database, use `POSIX_FADV_DONTNEED` on its
  dedicated volume files, restart it and execute one selected query without
  warmup. `cold_evict.py` normally needs `sudo -n` to read Docker's volume. Global
  host caches are never dropped. Storage-device caches remain uncontrolled.
  `first_touch` samples with an eviction receipt are the file-cache cold results.
- `EXPLAIN (ANALYZE, BUFFERS, WAL, SETTINGS, FORMAT JSON)`, relation/index bytes,
  index build time, full-schema reindex time, container CPU/memory/I/O observations
  and crash-to-ready-plus-count time. Durable writes remain enabled.

Statement timeout is five seconds for measured queries. A timed-out case is
recorded and remaining samples of that case are skipped; timeouts are never
converted into fast empty results or included as successful latency samples.
Complete correctness scans have a separate budget, increased from 180 to 600
seconds for the 32M validation. Each full set is materialized once and compared
both ways; it is checked before the warm profile, separately from concurrent
timing. Quantiles use
nearest rank and preserve sample counts and failures. Thirty warm observations
and ten cold observations have limited tail resolution; p99 is effectively the
slowest observation at these sample sizes.

The keyset anchor is resolved before timing, as a caller would already possess
it. The offset case skips one million rows (or half the corpus when smaller).
The compared operations are traversal methods, not identical page numbers.
Ordering uses `(published_at, info_hash, id)` to distinguish content siblings.
Concurrent inserts and mutable ordering fields still require a snapshot or a
documented high-water mark for repeatable export semantics.

## Scope and cleanup

The baseline models the relevant existing B-tree/GIN indexes. It does not install
all historical migrations or count every production index and history table.
The candidate tests raw-name GIN, ordered page indexes, an external-ID index and an episode
GIN index. Candidate inclusion is an experiment, not a recommendation to retain
every tested index. The decision and measured tradeoffs belong in the results.
PostgreSQL index variants are compared on the same authority and corpus; this
does not establish superiority over an untested external engine.

Raw JSONL is append-only. Keep failed or preliminary runs labeled separately.
The suite stops on failed correctness, resource preflight or recovery checks.
The final scale leaves its disposable database available for diagnosis. After
saving results, remove only these explicitly named resources:

```sh
docker rm -f dh-waypoint-catalogue-bench-test
docker volume rm dh-waypoint-catalogue-bench-test
docker network rm dh-waypoint-catalogue-bench-test
```
