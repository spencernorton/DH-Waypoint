# Catalogue search: authority, selective queries and scale qualification

## Decision boundary

PostgreSQL remains catalogue authority. First measure its existing B-tree and
inverted indexes against the required workload. A different engine must earn
its additional storage, synchronization and recovery cost in the same test;
an engine's release date does not establish a performance advantage.

The [disposable SQL harness](../../scripts/catalogue_bench/README.md) compares
the same synthetic corpus through raw-name, canonical-ID, external-ID, category,
episode and browse paths. Results belong with [the benchmark record](../project/benchmarks.md).
SQL latency is separate from end-to-end API latency and crawling yield.

## Facts, proposals and serving are different concerns

Raw metainfo, validated torrent identities and file inventory are catalogue
facts. A plugin's proposed type, match, title or language carries its processor
identity, source revision, version, confidence and verification/application
receipt. A proposal does not become authority merely because its confidence
number is high. Core validates the contract, source binding, candidate identity
and policy before accepting a mapping. Search consumes accepted facts; it never
calls a classifier or requires direct plugin database access.

Serving eligibility is checked against current catalogue authority before
sorting, grouping or limiting. An indexed identity, cached candidate or search
projection cannot override privacy, authentic quarantine, adult/name policy or
other configured exclusions. A plugin outage leaves discovery, indexing and
ordinary catalogue reads available; enrichment can catch up independently.

Unmapped, stale and uncertain annotations remain distinguishable. ID-only
search returns established mappings. It cannot certify an unclassified torrent
by guessing its identity. Generic text and raw-catalogue search remain separate
fallbacks. Raw-catalogue retrieval must also work when a content projection has
not yet been created.

## Query routing

Use `(content_type, content_source, content_id)` as a complete identity. Numeric
TMDB movie and TV IDs are different namespaces. v5.7.0 already supplies
`TorrentContentIdentityCriteria`; Torznab's typed TMDB path reuses it.

For external IDs, resolve a small set of canonical keys from authoritative
`content_attributes` with `key = 'id'`, source, value and content type. Probe
`torrent_contents` with that key set. Alternate titles and plugin annotations
with identical text are not identifiers. An `IN` semijoin also avoids row
multiplication when several aliases identify the same content. It gives the
planner an opportunity to drive release lookup from the small key set instead
of running a correlated OR subquery for every catalogue row.

The [Torznab supportedParams revision](https://torznab.github.io/spec-1.3-draft/revisions/1.1-Newznab-supportedParams.html)
combines supplied ID systems with OR. Categories, type, episode/date and any
supplied text still constrain the result. An unknown IMDb mapping must not veto
a known TMDB or TVDB mapping. Advertise only implemented capabilities.

The inspected client implementations are pinned rather than inferred from
product names:

- [Sonarr request generation](https://github.com/Sonarr/Sonarr/blob/b3d4bb8c18f7feb6744a6cd3231279a45351bb5e/src/NzbDrone.Core/Indexers/Newznab/NewznabRequestGenerator.cs)
  uses advertised TV IDs, season and episode parameters. It supports aggregated
  IDs and keeps title-search tiers. Daily shows use year plus `MM/DD`.
- [Radarr request generation](https://github.com/Radarr/Radarr/blob/56f6168bb6f254c9a34d0e5da76040dcb4215d3d/src/NzbDrone.Core/Indexers/Newznab/NewznabRequestGenerator.cs)
  uses TMDB/IMDb movie IDs and a title/year fallback tier.
- [Prowlarr request generation](https://github.com/Prowlarr/Prowlarr/blob/874e38b99e61c8c2da8f6e7c3d5128e002cc8b31/src/NzbDrone.Core/Indexers/Definitions/Newznab/NewznabRequestGenerator.cs)
  translates supported IDs and category mappings, and may send both text and IDs.

These clients also evaluate release names and attributes. There is no universal
`quality=` parameter that can safely be turned into a hard predicate for every
client. DH-Waypoint's implemented category subtypes can constrain resolution;
season packs must remain eligible for their contained episodes. The exact
supported parameters and profile limits are in the [API reference](../reference/torznab-api.md).

## Index and paging choices

Canonical release lookup already has the grouped-key index from migration
00036. Measure it before adding another identity index. A reverse external-ID
index can make the small-key lookup selective. Match B-tree leading equality
columns to actual predicates, as described by the
[PostgreSQL 16 multicolumn rules](https://www.postgresql.org/docs/16/indexes-multicolumn.html).

The existing content-name GIN index serves full-text search. Raw `torrents.tsv`
has a historical B-tree, which does not implement the `@@` full-text operator.
GIN is the [documented preferred text-search index](https://www.postgresql.org/docs/16/textsearch-indexes.html).
Benchmark raw GIN explicitly rather than assuming a content index covers
unprocessed source rows.

REST v2 instead implements literal case-insensitive substrings with escaped
`ILIKE`, a minimum three-character query and canonical-hash cursors. Migration
00071 supplies its `GIN(name gin_trgm_ops)` index, using PostgreSQL's
[trigram support for ILIKE](https://www.postgresql.org/docs/16/pgtrgm.html#PGTRGM-INDEX).
Token-based TSV results are
an alternative SQL workload, not a measurement of that API. Qualify substring
queries and trigram construction separately before promoting API expectations.

Ordered page indexes can avoid sorting a large candidate set for `LIMIT`.
The [ordering documentation](https://www.postgresql.org/docs/16/indexes-ordering.html)
explains that tradeoff. Extra indexes also consume bytes and make updates more
expensive; retain the ones supported by measurements, not every candidate.
An episode GIN index is useful only when the actual predicate can use its
operators. A function call or JSON subexpression does not automatically use a
whole-document GIN index.

Migration 00072 adds the complete published-page, typed-page and partial
external-ID lookup indexes. It uses
[concurrent construction](https://www.postgresql.org/docs/16/sql-createindex.html#SQL-CREATEINDEX-CONCURRENTLY)
outside a transaction. Concurrent construction still consumes I/O and disk,
and can wait for old transactions. A failed build may leave an invalid index;
inspect validity and remove that failed index before retrying the migration.
Qualify build time, temporary headroom and ingestion impact on a restored
catalogue before rollout. It removes the historical raw `torrents.tsv` B-tree,
which cannot serve `@@` and is not used by raw v2's `ILIKE` query. Rollback
restores that B-tree. Historical migration files stay intact.

Published content pages use `(published_at, info_hash, id)`. `id` distinguishes
several content rows belonging to the same torrent. A deterministic order fixes
ties in a stable snapshot; it does not freeze concurrent inserts or mutable
ordering fields. Export consumers need a snapshot or documented high-water
mark if they require repeatable traversal.

Offset remains part of the Torznab contract. It is expensive for deep scans
because [PostgreSQL must process skipped rows](https://www.postgresql.org/docs/16/queries-limit.html).
Prefer a tuple cursor for long catalogue traversals and use `limit + 1` for
has-next-page. Raw hash traversal can use the primary key without a count.
The harness excludes cursor-anchor discovery from keyset timing, since the
caller already has that anchor.

For a classified-plus-unclassified fallback, apply eligibility inside each
disjoint branch before taking its top K. Merging those bounded branches can
produce the same first page as the complete union. Limiting first and filtering
afterward can silently lose eligible rows. A fallback candidate must retain its
uncertain status rather than acquire a canonical-ID claim from its text match.

## Alternatives and limits

An optional search projection can remain rebuildable from PostgreSQL. If used,
publish its source revision and lag, revalidate candidates against current
eligibility, and refill pages after rejected or missing candidates. Define
behavior when the projection is missing, stale or rebuilding. Do not require a
plugin to share database credentials to maintain that projection.

[OpenSearch exact term fields](https://docs.opensearch.org/latest/query-dsl/term/term/)
can represent IDs and facets, while its
[refresh model](https://docs.opensearch.org/latest/api-reference/index-apis/refresh/)
introduces a visibility boundary for new writes. A second service therefore
needs synchronization, recovery and freshness qualification in addition to
query timings. No OpenSearch speed advantage is claimed by this SQL benchmark.

[SQLite FTS5 external-content tables](https://www.sqlite.org/fts5.html#external_content_and_contentless_tables)
offer a compact embedded projection, but their content/index consistency is
the application's responsibility. Rebuild, concurrency and eligibility costs
would need measurement on the same workload. They have not been qualified as
an authority replacement here.

The SQL harness uses a reduced schema and declared synthetic skew. Literal SQL
plans do not prove every application prepared plan:
[PostgreSQL can choose generic or custom plans](https://www.postgresql.org/docs/16/sql-prepare.html).
Actual API hydration, snapshot validation, public traffic, cold storage devices,
mixed v1/v2 hash widths and long-term bloat remain separate qualification axes.
No latency figure should be promoted to an end-to-end service guarantee without
that evidence.
