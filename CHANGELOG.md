# Changelog

## Unreleased

## 5.8.5 — 2026-10-27

- Broad grouped text pages stream ordered candidates through the complete
  guarded winner lookup, stopping when the page is full. This avoids checking
  every group in a materialized prefix before returning an ordinary page.
  Selective text sets retain their existing grouped strategy.
- Optional text-prefix work allows five seconds of bounded execution headroom,
  while retaining the caller's original deadline for exact fallback.
- Returned rows, winner ordering and same-snapshot short-page exhaustion remain
  authoritative. Deep common-text pages cap their initial prefix at the existing
  maximum, retaining an optional attempt before exact fallback.

## 5.8.4 — 2026-10-27

- A complete grouped page no longer repeats its candidate scan to count rows.
  Short pages still prove exhaustion in the same read snapshot before returning.
- Searched pages begin with a larger raw prefix to reduce repeated index walks.
  Optional text-prefix work has a three-second budget to accommodate planning,
  cold reads and page expansion before the exact fallback. The caller's original
  context, visibility checks, ordering and paging rules are retained.

## 5.8.3 — 2026-10-27

- Grouped pages choose a cheap candidate superset before checking each title's
  highest-seeded eligible representative. Ordered per-title trials use guarded
  point lookups; a trial containing no eligible release retains the complete
  group lookup. Returned records, visibility and representative selection remain
  authoritative.
- Small text-match sets are selected before language and other client facets,
  avoiding large bitmap-index intersections for selective searches. A bounded
  match probe is a planner hint only. Optional text work shares a one-second
  budget and retains the caller's original context for the exact fallback.
- Final short pages use candidate-exhaustion proof in the same read snapshot.
  Default ordering retains canonical group-key order; incompatible roots/joins
  and caller transactions retain the original path. Counts, facets, source and
  processing policies are unchanged.

## 5.8.2 — 2026-10-27

- Hash lookups now resolve requested identities and hybrid aliases into a
  small key set before fetching rows. Page hydration can use the existing
  identity indexes instead of scanning the catalogue; full v2 identities,
  direct keys and duplicate handling retain their existing behavior.
- Filename evidence uses literal contains checks for necessary ASCII
  substrings before invoking the authoritative regular expression. Unicode
  text and non-UTF-8 databases bypass this optional optimization, retaining
  their complete existing matching behavior.
- Selective text searches spend at most 500 ms on the optional ordered-prefix
  strategy before using the authoritative grouped query. Counts, winner
  selection, pagination and the caller's deadline are unchanged.

## 5.8.1 — 2026-10-26

- Classifier CEL `matches` calls no longer recompile their regex on every
  evaluation. Constant patterns compile once at program creation and the
  remaining patterns (keyword-list fold variables) go through a bounded
  compiled-pattern cache, cutting deterministic pipeline time roughly 3x
  (about 1,090 µs to 320 µs per benchmark run) and allocations by about
  89% with identical match and error semantics.
- Consolidated wiring: the queue purge worker moved from `internal/queueclean`
  into `internal/queue` (config section, worker key, logger and metric names
  unchanged; only the `config show` type label now reads `queue.PurgeConfig`),
  and nine single-purpose `*fx` wrapper packages were inlined into the app
  and cli packages with fx module names preserved. No provider, group,
  lifecycle, config key or CLI behaviour changes.

## 5.8.0 — 2026-10-26

- Serving queries avoid evaluating the full Unicode-aware filename evidence
  regex when a conservative necessary ASCII substring is absent. The original
  matcher still decides eligibility; optional or unsupported alternatives keep
  the existing evaluation path. This reduces repeated filename-check work in
  grouped and raw search without changing filters, counts or pagination.

- Locked processor source reads now take per-hash row locks instead of
  table-wide `SHARE` locks on the evidence, verdict and catalogue tables.
  Finishing a processor job no longer blocks evidence inserts or other
  writers for unrelated torrents.
- Peer wire metadata requests now dial both IPv4 and IPv6 addresses, close active sockets immediately when the request context is cancelled, and document the supported BitTorrent / BEP compatibility surface in `docs/protocol-compatibility.md`.
- The Compose healthchecks no longer probe `/metrics`, which returns 401 once
  `HTTP_SERVER_API_KEY` is set and so marked the container unhealthy. They
  probe the public `/status`. Both Compose stacks now probe `/status` with
  `--fail` and a 90-second start period because `/status` returns 503 for about
  the first minute (`docker-compose.yml` now defaults to `v5.8.0`).
  `observability/grafana-agent.config.river` now targets `dh-waypoint:3333`
  and labels Loki logs `job="dh-waypoint"`.
- The bundled Grafana dashboard's memory, classifier and queue panels were
  empty. Memory reads `bitmagnet_process_resident_memory_bytes`, the classifier
  panels, whose metrics no longer exist, show optional-processor outcomes and
  request latency, and the queue panel uses the selected Prometheus data
  source and plots job counts instead of `increase()` of a gauge. The latency
  panels show the recent median rather than the median since start.

- `/import` now streams newline-delimited JSON with a 1 MiB per-record limit, closes active import sessions cleanly on failure without deadlocking, and preserves HTTP socket write deadlines through response finalization.

- Fix the Compose quickstart, which has never started. Both Compose files
  passed an empty `TRACKER_PASSKEYS`, which every release since 5.6.0 rejects
  at startup ("tracker passkeys must be 8-256 characters"). They now pass it
  only when it is set, `.env.example` leaves it commented out, and an empty
  list setting now means no values instead of one empty value. CI starts the
  root Compose stack on every change.
- `/status` no longer reports the backend down because TMDB has no API key: an
  unconfigured TMDB check is inactive. The quickstart's `/status` returned 503.

- Compact DHT node and peer address decoders now reject truncated byte slices cleanly instead of panicking on malformed UDP payloads.

- `plugins/` now holds three templates, each shipped with `enabled: false`: a
  regex classifier, Armiger Ultra and OpenRouter. The root Compose file gains
  `armiger` and `openrouter` profiles whose services listen on the core's
  loopback, so neither needs a service token. `examples/plugins/` and
  `examples/armiger-processor/`, which pinned an Armiger image tag that is not
  on GHCR, are removed.
- Add `adapters/armiger`, a processor bridge to the published Armiger Ultra
  image's `/v1/classify`. Categories without a content type, low-confidence
  answers and an unreachable Armiger abstain or defer.
- The LLM adapter moves from `plugins/llm` to `adapters/llm` and ships an
  OpenRouter configuration. Its default prompt now treats the release name as
  evidence and other processors' abstentions as no evidence against a label. On
  twelve labelled names, gemini-2.5-flash-lite went from 5 to 12 correct and
  gpt-4o-mini from 2 to 11.
- `examples/llm-match-embeddings.yml`, an overlay for the retired in-core
  matcher whose keys now fail startup, moves to `_legacy/`.

- The Compose examples pin `v5.8.0` (`docker-compose.yml` defaults to `v5.8.0`) and pass `EVIDENCE_WEBHOOK_SECRET` from
  `.env`, so the *arr webhook can be authenticated as the README describes.
  Both stacks now read the same `.env.example` settings, which no longer sets
  `PROCESSING_MODE`: the root stack runs `basic` and the source stack `full`,
  as documented.

- Bound the optional grouped candidate-prefix strategy to two seconds for
  full-text searches, then use the existing exact grouped query while the
  caller's deadline remains live. Fast searches and unfiltered browsing retain
  their optimization; database errors and caller cancellation still propagate.

- Bound GraphQL body/document/parser work, expanded selections and aliases,
  page sizes, input lists and caller-selected count budgets before resolvers
  run, including cached/persisted operations. GraphQL accepts JSON POST only.
  GraphQL and REST catalogue work now inherits a 30-second context deadline
  with bounded socket reads/writes; header and idle timeouts protect the HTTP
  listener while separate streaming imports retain their existing behavior.
- Published content pages use `(published_at, info_hash, id)` for a complete
  order, including several content rows belonging to one torrent. Add a disposable
  catalogue SQL harness for deterministic scale, correctness, concurrent writes,
  cold/warm queries, index bytes, reindexing and recovery qualification.
- Identifier snapshot search now probes `torrent_contents` with one index
  equality per resolved canonical key instead of keeping the identifier
  subquery semijoin in the page query. Torznab requests carrying several
  external IDs (for example `tvdbid` with `tmdbid`) resolve their distinct
  key set inside the existing repeatable-read snapshot, covering IDs that
  map to the same canonical content and IDs attributed to several contents;
  wider alias fan-out keeps the previous complete predicate.
- Raw v2 substring search (`/api/v2/search?q=...`) no longer walks the
  primary-key index under a generic prepared plan. With `q` set it orders by
  `torrents.info_hash || ''::bytea`, which returns identical ordering and
  cursor pagination while letting the planner use the trigram name index for
  rare or absent terms; cursor browsing without `q` is unchanged. The
  catalogue benchmark's prepared substring case mirrors the new ordering, and
  its resolved-identifier case now resolves up to 32 canonical keys and pages
  with one equality arm per key inside the snapshot, falling back to the
  complete predicate only beyond that bound.
- Add concurrent indexes for ordered canonical identities and reverse external-ID
  lookup, selected after 2.4M/10M/32M synthetic qualification. Resolve singleton
  external aliases and read their pages in one read-only snapshot, preserving
  multiple-ID alternatives, caller cancellation and serving checks before pagination; raw v2
  substring indexing remains in migration 00071. Rollback removes only the two
  new indexes and preserves the current raw schema and existing downgrade holds.
- Torznab forwards HTTP request cancellation and deadlines to catalogue search,
  including external-identifier lookup and nested result hydration.

- REST search returns `total_count_is_estimate` alongside `total_count`, so
  clients can distinguish an approximate total from an exact count. The flag
  is always present, including `false`, and documented in OpenAPI. REST stats
  likewise returns `torrent_contents_is_estimate` with its catalogue total.
- Torznab uses the indexed canonical identity for TMDB searches and resolves
  external IDs through complete catalogue keys. Multiple IDs are alternatives,
  so an unmapped ID no longer rejects a match on another supplied ID. TV searches
  advertise and accept `tvdbid`. Only `id` attributes establish external identity;
  text search and the existing serving-policy checks remain available.

### Fixed

- Single-file `.exe` and `.7z` torrents masquerading with TV episode scene
  names are no longer hinted as `tv_show` by canonical or title evidence, and
  historical grabs of such releases no longer cast title-evidence votes. The
  core classifier now classifies them as `software` or leaves them unmatched
  instead of attaching TMDB/TVDB series. Optional processors proposing
  `movie`, `tv_show` or `xxx` for torrents whose file evidence contradicts
  video are rejected as `rejected_policy`, including when the content filter
  runs in shadow mode (`Enforce=false`).

### Breaking changes

- Retire `builtin/llm` and built-in online calibration. Inference, prompts,
  provider credentials and budgets now live in a separately runnable HTTP
  processor adapter (`adapters/llm`). Existing registrations fail closed with
  migration reason codes; retain history and replace them using the documented
  migration. Static regex weights no longer consume retained learned state.
- Default native processing to `basic`, matching Compose. Explicit
  `PROCESSING_MODE=full` retains the optional deterministic classifier.

### Added

- Standalone OpenAI-compatible LLM processor with local/hosted examples,
  two-stage inference opt-in, strict proposal validation, bounded calls and
  feedback receipts. GPT-5 family requests omit temperature.
- Check active Go syntax for direct provider HTTP calls and learning in core,
  as well as dependency names; enforce the adapter's public-contract boundary.


- Index v1, pure v2 and hybrid torrent metadata with complete SHA-256 identities,
  verified hybrid associations, original-info-byte hashing and bounded BEP 52
  validation. Add validated torrent-file import, a raw zero-plugin REST v2
  catalogue, complete magnets and explicit processor v2 negotiation. Preserve
  v1 API and processor semantics. Identity migrations refuse unsafe downgrade;
  raw name search gains a concurrent trigram index. Metadata indexing does not
  download payloads or claim unavailable BEP 9 outer piece layers were verified.

## 5.7.1 — 2026-10-24

- Seed scrape results are committed 250 hashes per transaction instead of the
  whole batch in one. Each transaction holds the admission SHARE lock on
  `label_evidence` and its torrents' row locks, so a large batch stalled every
  evidence writer until it finished. Each chunk still rechecks admission and
  commits its ledger, sources, derived counts and liveness together; a failure
  leaves earlier chunks committed.
- The *arr webhook allows its evidence insert 30 seconds instead of five. The
  insert can still wait behind a seed-scrape packet's admission lock, held for
  one tracker round trip, and *arr clients never retry a failed webhook, so a
  timeout lost the observation.
- The v2 processor's source lock gives up after two seconds whatever the
  caller's deadline, so a waiting SHARE request cannot hold back writers to its
  eight tables for longer; the rest of its transaction keeps the session's
  `lock_timeout`.

## 5.7.0 — 2026-10-24

- GraphQL torrent search accepts `contentIdentity: {type, source, id}` to
  retrieve a known title's releases through its indexed canonical identity.
  Filtering runs before sorting and pagination and retains ordinary serving
  policy, counts and facets. Existing text and hash searches are unchanged.

## 5.6.1 — 2026-10-22

- A classifier delete decided while catalogue recovery is disabled is now
  held without failing its `process_torrent` job. Ordinary rows in the batch
  were already committed and nothing is removed; failing the job only retried
  the same classification (including its LLM calls) for the same verdict and
  left a failed queue row per batch. The new
  `dh_waypoint_classifier_delete_held_total{content_type,rule}` counter
  records each held delete. Protected sources and the operator GraphQL delete
  still refuse with their existing errors.
- Build with Go 1.26.10 (`golang:1.26.10-alpine3.23@sha256:107301490755aa2ea1b0b43788b6e62cff2faae45a538028b193becc06aaffba`)
  for GO-2026-6809 in `crypto/x509`.

## 5.6.0 — 2026-10-18

- REST v1 read API: `GET /api/v1/search`, `/api/v1/torrents/:hash`,
  `/api/v1/torrents/:hash/files` and `/api/v1/stats` over the same
  serving-policy search as GraphQL, with stable snake_case JSON and
  an OpenAPI document served at `/api/v1/openapi.yaml`. UIs and
  scripts no longer need a GraphQL client to read the catalogue.
- Optional `HTTP_SERVER_API_KEY` gates GraphQL, `/import`,
  `/metrics` and `/api/v1/*` (`Authorization: Bearer` or
  `X-API-Key`). Surfaces with their own credentials (Torznab,
  tracker, processors, private API, evidence) are never
  double-gated; `/status` stays public; unset preserves the open
  trusted-network posture.
- Unknown keys in YAML configuration files now fail startup with the
  offending paths named, instead of being silently ignored.
  Environment variables are exempt; map-typed settings such as
  `classifier.keywords.*` remain open by design.
- Plugin-file ownership is persisted (`processor_registry.source`,
  migration `00069`): a plugin file deleted while the core is
  stopped is deregistered by the first sync after restart, closing
  the in-memory ownership caveat from 5.5.0.
- Tracker scrape `downloaded` is now an all-time per-peer completion
  total (`tracker_swarms`), counted exactly once per peer identity
  and surviving peer-row retention.

## 5.5.0 — 2026-10-18

- Add `PROCESSING_MODE` (`processing.mode`): `full` (default) keeps the
  complete CEL classifier workflow byte-identical; `basic` stamps only a
  coarse content type from file-extension byte majority with no metadata
  providers or network calls; `raw` persists metainfo untouched and runs
  neither classifier nor content filter. Installs whose classification
  and filtering live in processor plugins or a custom dataset can now
  turn the built-in pipeline down or off with one setting. The mode is
  part of the processor policy digest, so changing it stales
  outstanding processor pipelines. Name policy and the CSAM pre-fetch
  blocklist remain independent safety controls.
- Drag-in plugin files: the processors worker reconciles
  `*.plugin.yml` files from `PROCESSORS_PLUGINS_DIR` (default
  `plugins/`) into the processor registry every 30 seconds — adding a
  file registers, editing updates, deleting deregisters, with no
  rebuild or restart. API-created registrations are never modified by
  file sync. New CLI surface: `dh-waypoint plugin list|validate|test`,
  `dh-waypoint init` (scaffolds `config.yml`, `.env` secrets and a plugins
  directory) and `dh-waypoint doctor`. Six validated templates ship in
  `examples/plugins/` with guides for the API, plugin files, and
  connecting hosted or local LLMs.
- One-command install: a repository-root `docker-compose.yml` runs the
  published GHCR image (releases now also push `latest`), defaulting
  new installs to `basic` processing mode with `./plugins` mounted.
- Private torrents and a built-in tracker (all opt-in, off by default):
  `dh-waypoint torrent create` hashes files/directories into validated
  `.torrent` files (`--private` sets BEP-27); `POST
  /api/private/v1/torrents` uploads a torrent file through the shared
  ingestion gates and stores its exact bytes; downloads serve
  personalised copies with the announce URL rewritten outside the info
  dictionary (info hash unchanged); `GET /announce/<passkey>` and
  `/scrape/<passkey>` provide an HTTP tracker with BEP-23 compact
  peers. Private torrents remain excluded from GraphQL, Torznab,
  public tracker scraping and processor plugins. Migration `00068`
  adds the file and peer tables without a torrents foreign key, keeping
  the catalogue-recovery snapshot contract unchanged.
- Allow `priority` in processor `PATCH /registry/:id` control writes,
  so pipeline order can change at runtime under the existing
  generation CAS without re-registration or invalidating the
  execution generation.

## 5.4.0 — 2026-10-18

- Consult the measured language gate in the content filter's
  foreign-audio decision: a localized release title for an
  English-original work now drops under `drop_foreign_audio`, and a
  positive English-track reading vetoes the narrow scene-marker drop
  (dual-audio preservation). Filter defaults are unchanged (opt-in).
- Remove the dead `udp://exodus.desync.com:6969/announce` tracker from the
  default seeds scrape pool (0 successful scrapes in production
  measurement); operators can still add it back via `SEEDS_TRACKER_URLS`.
- Retain piece hashes for torrents an \*arr grabbed or imported. The
  qBittorrent poller exports each held public catalogue torrent once, verifies
  SHA-1(info) against the info hash and stores its piece length and hashes in
  `torrent_pieces`, so releases stay verifiable against their files after the
  client drops them. The crawl itself still stores no pieces by default.
- Add `internal/classifier/langgate`: deterministic English-audio evidence
  from release names, a byte-exact port of the Python reference rules v5
  (bare `DL`/WEB-DL no longer counts as an English track, dual-audio labels
  need their listed languages, `sub UKR` names subtitles, lower-case
  rus/ukr tags and Russian dub studios name non-English audio), proven
  against the reference on 69,346 names across all flag combinations. Also
  adds the Russian/Chinese title rule and detection of English-original
  works released under a localized non-English title. The content
  filter's foreign-audio decision consumes it (first entry above).
- Handle the remaining measured source-title shapes in the NoiseV2 parse:
  strip a leading broadcaster token (`PBS`, `BBC`, `ITV`) and one leading
  `[Group]` bracket before title extraction so neither glues onto
  `BaseTitle`, and prefer the parenthesised English title on `[Esp]`-tagged
  Spanish editions. Legacy parse output is unchanged.
- Normalise part spellings in title matching: `Pt. 1`, `Part II` and
  `Part One` now key as `part 1` / `part 2`.

## 5.3.0 — 2026-10-17

- Enrich processor jobs with optional public-safe swarm hints (`input.sources`
  and `input.swarm`): DHT and tracker-scrape counts, seed history, liveness
  status, and title-derived grab-outcome priors when available. Hints are
  frozen with the prepared step input; the stored source revision still
  digests only stable catalogue facts, so background scrape refreshes cannot
  stale a lease. Private indexer names and credential-bearing tracker URLs
  are never disclosed, and absent telemetry remains an honest unknown.
- Fix `CoreLiveness` to read `torrent_liveness.status`, prefer authoritative
  `torrent_tracker_seeds` counts when `tracker_known` is true, and report
  `tracker_count` only for authoritative scrapes.
- Parenthesize multi-expression `WHERE` criteria so `OR` category branches
  cannot escape surrounding Torznab filters, and route the Books category
  through the shared category criteria.

## 5.2.0 — 2026-10-17

- Add priority-first processor stacking (`Priority ASC`, stage tie-break, `ID ASC`)
  with dynamic `Selector` evaluation against live `TorrentContext` and single-pass
  multi-capability dispatch (`Job.Capabilities` + unified `Proposal` patch).
- Expose the authenticated Core Operations API (`/api/processors/v1/core/parse`,
  `search`, `liveness`, and `policy`) so processors can invoke deterministic core
  services without duplicating parsing, search, tracker/liveness or policy logic.
- Add zero-container built-in processor drivers (`builtin/filter-list`,
  `builtin/regex-weights`, `builtin/tracker-liveness` and `builtin/llm`) alongside
  external `http` processors under the same v1 registration and governance contract.
- Add four-tier bi-directional feedback and online learning (`operational_receipt`,
  `teacher_consensus`, `verified_import` and `human_gold`) with pull cursor pagination,
  optional HTTP `/v1/feedback` push delivery, and persistent `learned_state` calibration.
- Support direct JSON unmarshaling on `model.Null*` and `model.Date` and PostgreSQL
  default `intervalstyle` day/hour scanning in `model.Duration.Scan`.

## 5.1.0 — 2026-10-16

- Measure a complete latest-500 terminal attempt window for each processor
  execution generation. Retention gaps and interrupted work remain explicitly
  unavailable; expired dispatched leases are recorded as technical failures.
- Add negotiated, audited whole-pipeline percentage admission with reproducible
  hash cohorts, atomic generation checks and stale-result rejection on changes.
- Export bounded retained job evidence with versioned input/result lineage,
  attempt histories and explicit raw-payload and reference coverage gaps.
- Reprocess retained decisions through the ordinary PostgreSQL processing
  queue with correlated receipts, current authority checks, idempotent requests,
  cancellation and preserved before-images. Requests cannot delete torrents,
  clear policy restrictions or silently rematch established identities.
- Retry local processor preparation failures within a separate durable bound;
  preserve unchanged input and distinguish exhausted technical failure from
  stale work without inventing plugin dispatches or applications.

## 5.0.0 — 2026-10-16

- Separate optional processors behind the generic v1 HTTP service contract;
  dynamic version-pinned registration, ordered stages, PostgreSQL leases,
  bounded retries/capacity, cancellation and stale-result rejection.
- Remove embedded model/provider runtimes, prompts, credentials and evaluation
  commands from the executable dependency path. Core discovery, deterministic
  processing, catalogue protection, search and indexer APIs remain independent.
- Freeze outstanding legacy inference jobs with complete before-images while
  retaining historical results, provider fences and recovery evidence.
- Publish authenticated generic status/job/configuration APIs, measured metrics,
  portable conformance fixtures, two independent synthetic processor examples,
  and migration/rollback procedures. UI and model packaging remain separate.

- Keep matcher provider, rate-limit and catalogue-resolution failures retryable;
  retain attempted queries and candidate details in shadow evaluation exports.
- Emit standard `tmdbid` and `tvdbid` Torznab attributes for Arr consumers,
  preserving the legacy `tmdb` attribute and rejecting malformed identifiers.

## 4.6.0 — 2026-10-12

- Clarify backend setup, test instructions and classification documentation; correct the Radarr UHD category to 2045.
- Integrate catalogue recovery snapshot and deletion foundation (`internal/cataloguerecovery`).
- Align container builder image with Go 1.26.9 on Alpine 3.23 (`golang:1.26.9-alpine3.23@sha256:6750308561953d451210ed1a8086d36c8c8bdcbf672d38d9d0d424b8a7a0216c`).
- Enforce immutable snapshot capture and verification during catalogue recovery.
## 4.5.2 — 2026-10-08

- Reserve half of each bounded seed batch for stale rechecks so continuous new
  discovery cannot starve previously checked public torrents. Return unused
  capacity to either class while retaining admission guards and scrape limits.
- Report recovery-disabled quarantine as an explicit held cycle instead of an
  operational error, retaining judgments and the existing removal guard.

## 4.5.1 — 2026-10-07

- Normalize independent qB source and category privacy facts during legacy
  quarantine restore. Refuse protected restores atomically while retaining
  their snapshots, raw rows, queued work and verdict history.

## 4.5.0 — 2026-10-07

- Enforce native and independently recorded private authority throughout public
  lists, counts, files, tags and Torznab, matching the existing denied-grab
  boundary. Retain source rows and ordinary wanted/manual lookalikes.

- Add an optional, default-off release-name policy shared by acquisition,
  import, processing and model admission. Exclude exact Han/Cyrillic names,
  clear adult composites and configured hashes while retaining raw sources.
  Recheck current names at provider and application boundaries, retain paid
  responses/fences after denial, and offer a bounded internal name-only check.
- Normalize independent qB privacy source/category facts consistently at
  direct, queued and final application gates, including padded Unicode space.
- Keep known private hashes out of public tracker requests. Apply the opt-in
  release-name and current XXX policy before seed selection, each bounded
  packet and atomic ledger/source/count/liveness persistence, including CLI
  refreshes. Retain public wanted eligibility and existing scrape limits.

- Preserve tracker-positive history and standing coverage for leechers-only
  swarms, matching the existing seeder-or-leecher live signal. Keep known zero
  and failed coverage separate from confirmed-dead claims.
- Add an opt-in release-name eligibility policy shared by consumer views,
  public hash checks and metadata repair. Han/Cyrillic names, clear adult
  combinations and explicitly configured hashes stay ineligible without
  deleting stored data or changing audio/origin metadata.
- Normalize independent privacy evidence when freezing or applying metadata
  repairs, including protected facts committed after the plan was frozen.

- Select grouped pages from bounded ordered eligible prefixes, retaining the
  exact highest-seeder representative and falling back to the complete grouped
  query for short or unsupported shapes. Add an ordered matched paging index.
  Avoid disproportionate JIT compilation only inside each prefix's owned
  read-only transaction. Existing caller transactions keep the original path.

- Measure matcher HTTP latency through bounded response reading and envelope
  decoding. Count model/stage outcomes and failures consistently for ordinary,
  controlled and embedding attempts, excluding admission, cache and replay.
- Fail closed on unqualified catalogue removal and crawler blocks. Route
  processor and content-type purge removal through the complete recovery
  writer, retain ordinary keep/review work, and count fully successful calls.
- Hold legacy retention and both junk-quarantine writers before partial capture
  can discard source rows. Retain paid Batch receipts, historical quarantine
  maintenance and restore support without reporting held actions as applied.
- Refuse legacy archive hard purge and protect operator restoration against
  competing private/override/verdict authority and mismatched source hashes.

- Recognize adult facet selections and aggregation buckets already excluded by
  mandatory serving policy without scanning global evidence. Preserve ordinary
  query plans, every eligibility guard and OR-facet self-aggregation.

## 4.4.0 — 2026-10-06

- Repair missing codec claims only from the exact parser proposal, retaining
  existing codecs and catalogue identities with journalled application and
  nullable search-projection rollback.

- Share qualified manual/reference/bitgrab tag protection plus wanted maintenance
  protection while preserving wanted-only matcher eligibility. Freeze
  independent authority before recovery snapshots and keep later private facts
  from using a stored crawler unblock exception.

- Refuse recovery-schema downgrade while any snapshot, audit event or consumed
  budget remains, including restored and expired snapshots. Empty-only
  downgrade serializes with recovery writers and remains retryable.

- Add an unwired, disabled recovery foundation that atomically captures complete
  catalogue sources, removal and a bound block receipt, with bounded storage
  guards and qualified operator restoration through the native crawler bloom.

- Add a bounded, local English and availability shadow-review command with
  source-bound receipts; preserve unknown track evidence and stale swarm data.
- Stop labelling unnamed dual/multi audio as English, preserving uncertainty
  in the foreign-audio filter, and version the matcher extraction contract.

- Add frozen, journalled field-only release repairs with NULL-only claim filling, reviewed historical source corrections and guarded rollback. Preserve retained claims during ordinary processing.

- Retain nullable, versioned HDR, audio/channel, encoder and revision filename
  claims with source provenance. Expose explicit claim filters and read-only
  GraphQL/Torznab fields, without changing catalogue identity or verifying tracks.
- Recognize explicit BDREMUX as a REMUX modifier and CAMRip as a CAM source.

- Apply deferred model receipts with source-bound compare-and-set transactions and retain committed facts across ordinary refreshes and response expiry.

- Add an opt-in durable request dispatch fence and bounded replay of retained
  first responses. Known pre-dispatch deferrals remain retryable; uncertain
  dispatches stay held even after ordinary capture retention.

- Add a disabled-by-default lifecycle for bounded optional model tasks, with
  source/policy identities, paced time-stratum admission, completion priority,
  owned worker leases and targeted transactional application. Ingestion retains
  its deterministic result while optional work waits for unchanged allowances.

## 4.3.0 — 2026-10-06

- Add reversible adult-content exclusion for consumer APIs with
  `SERVING_EXCLUDE_ADULT`. Known XXX, positive attached adult metadata and the
  core classifier's vetted strong native evidence are excluded before search
  pagination, grouping, counts and facets. Internal processing and recovery
  retain the stored data; the option defaults off.
- Prevent source-complete quarantines from reappearing in consumer searches
  after recrawl, independently of the optional verdict-reader mode. Legacy
  incomplete markers retain their review status; restoring an ordinary
  quarantine restores visibility when its snapshot row is removed.
- Keep concurrent item/count/facet criteria immutable and apply mandatory
  base predicates inside grouped and CTE queries without replaying them against
  the derived outer table.
- Recognize explicit HEVC/H.265 and AV1 codec claims while preserving existing
  serialized codec values. Infer supported scene groups independently of codec
  recognition and retain unknown or contradictory metadata as unknown.

- Parse explicit `WEB-DL`, `WEB.DL` and `WEBDL` source tokens consistently,
  preferring compound source aliases before their shorter prefixes.
- Parse release attributes and resolve unique local identities after a new
  movie/TV type fallback, within the existing content policy. The continuation
  runs once and makes no additional model or metadata API request.
- Commit bounded quarantine expiry chunks together with their verdict state and
  audit events. Restore and explicit deletion commit their ledger transition
  atomically; failed chunks retain snapshots and remain retryable.

- Bind matcher extraction cache entries to rendered file evidence and request
  routing. Group responses have a separate exact-request cache and cannot warm
  single extraction. Backlog matching refuses grouped warming when evaluation
  capture is disabled; select a single-request chunk explicitly instead. Safe
  grouped workflow consumption requires a separate provenance-aware path.

- Preserve authentic torrent source metadata in quarantine snapshots and restore
  it without overwriting newer observations. Legacy snapshots remain usable.
- Record explicit restores as local provenance with unknown swarm counts, so
  the default Torznab freshness window can surface them without bypassing
  tracker-zero, liveness or verdict exclusions.
- Commit restore and rematch enqueue together. Queue failures retain the
  recovery snapshot; migration 55 refuses to discard retained source snapshots.

- Bound type fallback application with an audited category allowlist. A
  disallowed prediction remains unknown without triggering that category's
  deletion policy; changed policy invalidates cached application.
- Add an audited language review action that persists a model-attributed tag
  while retaining the torrent and ordinary deterministic filtering behavior.
- Document bounded application, actual-effect checks and independent rollback
  controls for optional model stages.

- Run optional type inference inside the classification workflow's exclusion
  policy. Deterministic outcomes remain authoritative; custom workflows without
  the policy boundary cannot apply a live prediction.
- Separate language-model enforcement from deterministic filtering with
  `CONTENT_FILTER_LLM_ENFORCE`, retaining legacy inheritance when unset. Skip
  model language inference for advertised English-inclusive releases and block
  model application when audit or model evidence is incomplete.
- Retain the actual grouped junk request, first response and position-bound
  decisions. Incomplete cycles cannot quarantine; manual deletion and blacklist
  updates commit atomically against an existing quarantine snapshot.
- Add an optional embedding shortlist before the chat matcher. It uses an
  explicit independent route, bounded vectors and the shared durable allowance.
  Full candidate identity checks and chat confidence still govern attachment.
- Reject contradictory, duplicate, incomplete and refused model responses in
  runtime and evaluation. Ambiguous batch IDs abstain instead of choosing a row.
- Add private offline blinded review packets and diagnostic scoring. These
  artifacts never grant production authority or replace independent gold,
  calibration and prospective acceptance. New live modes remain off by default.

- Make the public backend independently buildable and deployable without a UI.
  Remove the Python site and its Go worker, browser GraphQL playground and
  site-only documentation. Keep DHT processing, matching, classification,
  evidence, Torznab, GraphQL POST, metrics, CLI and all PostgreSQL migrations.
- Separate backend releases with a root `VERSION` file and Go-only container.
  Public examples and CI now validate the headless deployment boundary.
- Breaking packaging change for 4.0.0: the former `ui` worker, port 8080 and
  SQLite/site configuration are no longer shipped. Existing API deployments
  keep their schemas; operate browser applications as separate services.

## 3.5.0 — 2026-10-03

- Add private `music` and `generic` leaf releases to the offline publisher,
  protected catalogue, member library and private Torznab feed. Explicit
  `--include-music` Plex discovery includes track versions; Music and Other
  use Audio 3000 and Other 8000.
  Music supports text search only. Keep version-1 video manifests, torrent
  layouts, private key permissions and fresh seed verification unchanged.

- Align administration with discovery's shared teal and navy palette, lime
  product mark, typography, navigation and card treatments in both themes.
- Support an HTTPS library-origin `/admin` proxy with an explicit, fixed
  `OPERATOR_INGRESS_HOST`. Prefix operator API requests and navigation while
  retaining disjoint host allowlists, verified operator roles and owner CSRF
  checks. The proxy must strip `/admin` and set its fixed operator Host.
- Add optional `INVITATION_BRIDGE_INGRESS_HOST` for canonical-origin machine
  enrollment behind a fixed operator transport Host. The empty default keeps
  legacy exact-host behavior; startup seals, proof and origin signatures remain
  required, and the feature remains disabled by default.

- Show a freshly generated invitation code alongside its link, with a
  separate copy action and a clear account-menu entry for code generation.
  Clear both secrets together when hidden, refreshed or leaving the page.

- Add an explicit, disabled-by-default owner action to admit their own
  invitation issuer before private indexing. Bind it to the verified account
  on the canonical operator origin; retain suspended memberships and all
  existing account, key and private permissions.

## 3.3.0 — 2026-10-01

- Add explicit `openai`/`ollama` chat backend selection for the TMDB matcher
  and type fallback. Keep `openai` as the default; Ollama requests use
  `max_tokens` and `reasoning_effort: "none"` with the existing requested caps.
  Reject incompatible provider and endpoint settings without changing stage
  enablement, live gates, budgets or privacy checks. Backend compatibility does
  not establish model accuracy or calibrated confidence.

## 3.2.1 — 2026-10-01

- Add manual invite-code entry and an explicit, disabled-by-default site
  registration mode so profile enrollment can run before private indexing.
  Preserve signed enrollment, issuer membership and suspension checks, while
  private routes and payment checkout remain closed in public-only mode.
- Partition site-registration preview limits by a proven proxy's canonical
  original connection address. Invalid or untrusted headers retain the
  transport-peer limit and never supply identity or membership authority.

## 3.2.0 — 2026-10-01

- Apply startup vault settings as one validated batch. A rejected payload keeps
  the original configuration intact, including operator roles and credentials.
- Connect the user library and operator console with an admin-only Admin
  navigation entry and a Back to library link. Destinations use validated,
  startup-only configuration or the existing host allowlists; authorization
  is still checked independently on the destination surface.
- Organize Settings into four workflow groups with typed logging, URL and
  secret controls, grouped deployment references and reduced-motion-aware
  section transitions.
- Align AI observations with the core's token labels and type-classifier
  metrics. Preserve missing counters as unknown, expose source observations,
  and label cost totals and sampled projections as estimates rather than
  invoices or model accuracy.
- Refine the operator console with bookmarkable navigation, keyboard-safe mobile
  navigation and per-field draft preservation.
  Runtime overrides expose save/reset progress and failures, with an explicit
  reset step and no browser persistence of replacement secrets.
- Show loading, stale, measuring, unavailable and failed operational observations
  alongside their values. Suppress live indicators on stale snapshots, distinguish
  aggregate response time from source observation time, and leave gaps in grab
  trends where no ratio was measured.

## 3.1.0 — 2026-10-01

- Automatically open profile enrollment for every invitation recipient when the optional
  enrollment bridge is enabled, including existing signed-in users. Require
  that path on the backend and support a signed site-local account provider
  alongside SSO; profile and password handling remain in the identity service.

## 3.0.0 — 2026-09-30

- **Breaking:** All existing and new generic personal indexer keys default to
  public-only access. Existing private-client integrations must explicitly
  rotate to a private-enabled key through verified human membership, then
  update their indexer connections and personalized torrent credentials.
- Private responses and tracker peers recheck the key capability, ownership,
  revocation and membership. Public DHT integration keys remain independent
  of private membership.
- Human-member private catalogue browsing retains its membership policy;
  the additional key permission controls personal-key private Torznab and
  personalized torrent/tracker credential issuance.

## 2.15.0 — 2026-09-30

- Add an optional explicit SSO enrollment subject format while preserving
  legacy account IDs by default. Bind enrollments to their startup format,
  reject cross-format membership collisions and require a separate reviewed
  account-data migration before enabling canonical principal subjects.

## 2.14.0 — 2026-09-30

- Add optional single-use member invitations with a permanent UTC-year
  allowance, exact-subject owner limits, revocation and atomic SSO redemption.
  Preserve existing account IDs; invitations do not enroll network peers or
  establish content readiness. Paid-credit receipts are separate and idempotent.
- Add a separate optional, purpose-signed SSO enrollment bridge with bounded
  replay state and exact principal binding. Preserve denial history and keep
  provider approval, project-capability activation and network admission under
  their own verified boundaries.
- Add disabled-by-default hosted Stripe Checkout for one fixed invitation
  credit, with durable orders, signed event reconciliation and terminal
  refund/dispute withdrawal. Browser returns grant no credit; uncertain
  outcomes retain their original payment key and require reconciliation.

## 2.13.1 — 2026-09-30

- Refresh private-seed availability in bounded publication-order batches with
  short SQLite transactions and original observation timestamps. Withdraw the
  complete proof epoch on failures, cancellation or deadline expiry; recovery
  cannot reactivate old proofs or an in-flight withdrawn/re-published release.
- Enforce one private-readiness application owner on a protected local POSIX
  database, while keeping disabled-default imports portable. Recheck exact
  proofs before private results, torrent/magnet issuance and tracker writes.
- Index expired private peers so tracker cleanup does not scan the full ledger.

## 2.13.0 — 2026-09-30

- Include all UI runtime modules, templates and static assets in installed wheels.
- Add separate, searchable streaming-service and TV/cable network browsers with
  logo tiles, accessible selection controls, and paged network discovery.
- Recover streaming logos across movie and series catalogues, try safe alternate
  images, and retain named tiles when an upstream logo is unavailable.
- Group subscription plans and storefront channels into one service option;
  search their combined regional catalogue and retain variant-name searches
  and existing provider links.
- Filter network series and title searches by verified TMDB network identities;
  preserve network selections in shareable URLs and browser history.
- Bound complete metadata requests and network export downloads, including
  queued work, to keep upstream failures responsive.
- Refine library navigation, typography, spacing and responsive discovery;
  show a compact service roster with full-catalogue search and disclosure.
- Add clear catalogue headings and loading, empty and unavailable states;
  retain selected brands and keyboard focus during discovery changes.
- Keep title identity visible when artwork fails and bring mobile release
  actions forward with expandable quality, source and edition filters.
- Preserve the full name and separate identity of The Roku Channel.
- Add account-scoped display, search and magnet preferences with clear save,
  browser-storage and retry states; preserve explicit shared-link filters.
- Show copied/opened/exported activity, tracked dates, observation time and
  seven/thirty-day UTC windows with explicit coverage notices.
- Deduplicate retried activity receipts and isolate delayed account responses;
  prevent anonymous/shared credentials from becoming personal usage totals.
- Add bounded operator catalogue pages and optional import acknowledgments
  that reconcile every source alias with its canonical private torrent.
- Keep catalogue scans consistent across readiness refreshes, metadata edits,
  process restarts and restored database snapshots.
- Add optional short-lived device address ownership for private tracker
  credentials, with authenticated seed snapshots, expiry and replay checks,
  and current-member filtering of advertised peers and swarm counts.
- Keep private tracker byte counters monotonic within active peer sessions and
  return full client-reported account totals beyond the release display limit.

## 2.12.1 — 2026-09-30

- Recognize qBittorrent 5.2 login responses and session cookies when checking
  private seeder readiness. Keep failed authentication, unusable cookies and
  incomplete seed copies unavailable.

## 2.12.0 — 2026-09-30

- Add an optional private media catalog alongside the public DHT catalog, with
  grants for existing SSO identities, revocable member tracker credentials,
  personalized private torrents and separate client-reported transfer metrics.
- Prepare and verify real movie, episode, season and show torrents from a
  read-only Plex inventory or explicit file manifest; advertise only freshly
  verified seed copies and retain separate metadata aliases for shared swarms.
- Add a private library and membership administration page, private Torznab
  feed, and candidate Jackett definitions for both catalog sources.
- Keep credential-bearing request URLs out of application access logs.
- Stream large Plex inventories into bounded catalog shards with resumable hashing,
  atomic publication generations, and independent full-file verification.
- Add optional shared read-rate limits for publication and verification hashing,
  without throttling metadata, file-stat checks, or unchanged cache reuse.
- Fail closed on interrupted inventory pagination, unavailable seed copies, and
  stale readiness after process restart; require verified seed registration before
  advertising private releases.

### Security

- Update pgx, CEL, mapstructure, Ed25519, compression and Go crypto/network/Unicode
  dependencies to their fixed releases, with required transitive module updates.
  Use the supported Go 1.26.8 toolchain in development, CI and the pinned container
  build; check called Go dependencies for known vulnerabilities in public CI.

## 2.11.0 — 2026-09-30

- Redesign the public library with colorful discovery navigation, animated cards,
  responsive filters, light/dark themes, and reduced-motion support.
- Surface advanced search as removable filters and preserve shareable browse URLs.
- Replace the introductory card with a rotating popular/new movie and series
  artwork banner, with pause controls and direct indexed-release lookup.
- Add cached streaming-provider catalogues, country selection, provider search,
  and provider-specific movie/series search with explicit availability checks.
- Add individual magnet copy/open actions and a reviewable collection for titles,
  seasons, shows, and manually selected releases, with copy and text export.
- Replace the collection prompt with direct title/show/season magnet copy controls.
- Persist private per-account aggregate grab and successful Torznab-search totals;
  show transfer and hit-and-run metrics as unreported until client reporting exists.
- Avoid retaining Torznab query-string credentials in raw UI access/error logs.
- Page title releases within explicit bounds and disclose partial results.
- Preserve collection history, dialog focus, and title identity during async metadata loads.
- Verify Linux amd64 release images against their source revision and UI version;
  exclude local credentials, environments, and state from Docker builds.

## 2.10.9 — 2026-09-27

- Establish GitHub pull requests as the development workflow, with privacy checks.
- Add deployment, configuration, security, upgrade and recovery documentation.
- Remove the inherited shared metadata key and automatic fallback; enrichment
  requires the operator's own TMDB_API_KEY.

- Fail closed when retained content-filter evidence loses its privacy, expiry
  or source-identity admission, with PostgreSQL regression coverage.
