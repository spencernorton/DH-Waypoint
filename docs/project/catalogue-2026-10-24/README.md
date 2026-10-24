# Synthetic catalogue qualification — 2026-10-24

This run measures PostgreSQL query and index alternatives on the same invented
catalogue. It does not compare complete search engines, measure HTTP latency,
classify real releases or establish production capacity. The
[design record](../../design/catalogue-search.md) explains the authority and
serving boundaries; the [harness](../../../scripts/catalogue_bench/README.md)
contains reproducible commands.

## Population and execution

The completed primary scales are recorded by `scale_complete` in the raw suite
events. Results at each scale use the same deterministic prefix, with one
content projection per torrent and 2.35 file rows per torrent: 95% single-file,
4% ten-file and 1% hundred-file records. Five percent share a hot identity; the
tail spans about one million identities. Seventy percent have accepted mappings;
the remainder is split equally among stale proposals, uncertain proposals and
missing proposals. The attributed proposals are separate from canonical facts.

The database was PostgreSQL 16.15, capped at six CPU cores and 16 GiB RAM with
no extra swap, 2 GiB shared buffers, 16 MiB work memory, 512 MiB maintenance
memory and durable WAL settings. Four workers populate the fixture. Large growth
rebuilds secondary query indexes afterward while retaining primary/unique/FK
constraints; steady ingestion is measured with the complete tested index set.
The client process is outside the database container's resource accounting.

The schema models relevant search indexes and serving predicates. It does not
install the complete production schema or account for every history table,
snapshot receipt, index or content sibling. The hashes are 20-byte synthetic
identities; mixed v1/v2 widths and file-heavy populations need separate tests.
Raw-name retrieval includes a missing-content-projection regression.

## Index sets and queries

Baseline retains the relevant historical B-tree/GIN indexes. Candidate adds raw
TSV GIN, complete published and typed-page B-trees, an external-ID reverse
index and whole-episode GIN. Selected is a subsequent experiment that replaces
the raw TSV experiment with migration 00071's name trigram GIN, removes the
unused raw B-tree and episode GIN, and adds ordered canonical-ID lookup.
The selected experiment must be distinguished from the indexes actually shipped
in migration 00072; the decision is recorded below.

The fixed main mix has 25 SQL cases: hash, canonical and external identity,
common/rare/missing full-text terms, type, resolution, season/episode/pack,
grouping, deep offset, tuple cursor and full/bounded unknown fallback. Every
case applies the reduced current privacy/quarantine/adult model before LIMIT.
Thirty warm observations are scheduled per case; four query workers repeat the
mix alongside actual source reobservations and inserts. A synthetic tail is
removed afterward, in FK order, and the exact initial population is restored.

REST v2 uses escaped literal `ILIKE`, canonical-hash order and `limit + 1`.
Its separate substring profile tests common, rare, absent and literal-wildcard
queries with 101-row retrieval. TSV measurements are an alternative query,
not a measurement of that API. Prepared probes test automatic/custom/generic
plans and a combined canonical-ID/episode/quality predicate, recording server
plan counters. Forcing a generic plan does not prove that automatic mode chose
it in an application.

## Observations and decision

Final results are populated only after the complete qualification finishes.

## Correctness and measurement limits

Complete `EXCEPT` comparisons run in both directions for equivalent query
shapes. The bounded fallback must preserve the exact first page and ordering.
An independent modular-arithmetic oracle checks complete rare and combined
filtered pages, including incorrectly empty results. Core serving and actual
request-option regressions run separately against PostgreSQL in CI. Accepted
IDs retain complete type/source/id namespaces; uncertain candidates never gain
an identity claim from text. ID-only requests cannot certify unmapped records;
the raw catalogue remains separately searchable without a plugin.

Measured SQL statements have a five-second budget. A timed-out warmup skips
that case's later observations; worker timeouts and skipped observations remain
visible. Quantiles include successful observations only, with failures and
sample counts beside them. Nearest-rank p99 is effectively the maximum at
30 warm or 10 cold observations. These samples cannot establish a service SLO.
The cursor anchor is resolved outside the timer. Offset and cursor timings
describe traversal methods, not the same page position.

Each cold observation stops only the labeled disposable container, applies
`POSIX_FADV_DONTNEED` to its dedicated PostgreSQL volume, restarts it, then
executes one first-touch query. The operation does not drop host-wide caches;
storage-device caches remain uncontrolled. Eviction receipts are retained.

Resource sampling is approximately every seven seconds, including Docker's
sampling latency. Docker working set excludes inactive file cache. Charged
cgroup memory and peak include that cache; those counters were added late in
the 10M run, so earlier coverage remains missing. Short stages can have no
resource sample. CPU percentage uses 100% per core, with a six-core cap.

Reindex timings use offline `REINDEX SCHEMA` on the reduced schema. Concurrent
index construction is measured on a quiescent synthetic corpus; it does not
qualify construction under live ingestion. Recovery is a quiescent forced kill,
restart and exact row-count/sentinel check, including count time. It does not
measure restoration from backup, in-flight transaction loss or HA failover.

Setup/correctness/maintenance use budgets separate from measured queries.
Development failures included an unsuitable initial query shape, an insufficient
tail-cleanup budget, and an insufficient cursor-anchor setup budget. They were
corrected before resumed final stages; no failed setup is labeled a completed
scale. Stage hashes and frozen source retain the implementation changes.

No 100M run, full HTTP/DHT throughput test, long-term bloat study or BitMagnet,
SQLite or OpenSearch engine head-to-head was performed. A different engine's
performance and synchronization/rebuild cost remain unqualified.
