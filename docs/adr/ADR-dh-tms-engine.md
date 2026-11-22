# ADR: DH-TMS Storage & Retrieval Engine Selection (Gate 0 Architecture Bake-Off)

- **Status:** Accepted (Pending Phase 1 Sign-Off)
- **Date:** 2026-11-22
- **Scope:** Core storage, search, Torznab/RSS, facet, dashboard statistics, file-tree hydration, fail-closed serving policy, and swarm telemetry engine (`DH-TMS`) integrated into `DH-Waypoint` via a PostgreSQL-coordinated dual-plane architecture.

---

## 1. Context & Problem Statement

`DH-Waypoint` serves BitTorrent metadata over REST, GraphQL, Torznab/RSS, and Stremio interfaces while enforcing multi-predicate fail-closed consumer visibility (`internal/serving.Policy`, `internal/namepolicy`, `internal/catalogueguard`, `processor_publication_reviews`, `processor_payload_reviews`, `junkpurge_quarantine`, `processor_exclusions`, `processor_publication_holds`, and adult media-dominance checks).

At 10.8M+ torrents and 133M+ file entries under a strict 6 GiB memory budget (`--memory=6g --memory-swap=6g`), evaluating full `serving.Policy` predicates inline inside PostgreSQL 18.6 (`pg_search` BM25 + GIN `tsv` + `pg_trgm`) exhibits severe tail-latency degradation and timeouts:

- Fresh zero-bloat restored PostgreSQL 18.6 footprint at 1× (`10.82M` torrents, `133.4M` files): **72.34 GiB** (growing >180 GiB with live write/scrape churn).
- Under a strict 6 GiB Linux cgroup v2 cap with real un-materialized `serving.Policy` SQL (`15s` statement timeout):
  - `cgroup memory.peak` pins flat at **6.000 GiB**.
  - Warm p99 latencies: `title_and_pub` (GIN) **3,842.5 ms** (1 timeout), `title_and_bm25_pub` (ParadeDB BM25) **573.9 ms** (1 timeout), `torznab_tvsearch_pub` **404.1 ms**, `facet_counts_pub` **3,771.0 ms**.
  - Hard timeouts (`>15,000 ms`): `prowlarr_tv_rss_pub` **10/10**, `dashstats_30d_pub` **5/5**, `substring_trgm_base` **5/10**.

**Architecture & Framing:**
`DH-TMS` (DH Torrent Metadata Store) is a **purpose-built, compact, `mmap`-first BitTorrent metadata store and retrieval engine** combining established IR and columnar storage techniques with BitTorrent-specific release grammar, structural Zstd-dictionary file-tree compression, and fail-closed serving policy enforcement. It is **not** a general-purpose SQL database and does not claim novel database primitives.

PostgreSQL 18 remains the ACID source of truth for governance, review approvals, payload receipts, and audit history (**`DH-TMS Dual`**). `DH-TMS` delivers 100% of its storage, search, Torznab/RSS, fallback, `dashstats`, fail-closed policy, and swarm telemetry gains with **zero ML and zero external processor installed**, preserving `scripts/check_processor_boundary.py` strictly (MIT-licensed Rust core + Go client; optional external processor hooks remain strictly on `processorapi/v1`).

---

## 2. Gate 0 Bake-Off Candidates & Decision Rule

Before committing to a custom storage engine, two hardened candidates sharing identical MIT Rust core planes were built and benchmarked head-to-head on the same real `1×` (`10,824,399` torrents, `133,388,349` files) and `3×` (`32,473,197` docs) datasets populated from **all 12 real PostgreSQL governance tables** (zero synthetic hash-tail policy flags), verified against a PostgreSQL 18.6 oracle with exact per-query infohash-set parity checks, a 500-query unlinked fallback recall audit, and strict Linux cgroup v2 `MemoryMax=6G` / `MemorySwapMax=0` enforcement with full host page-cache eviction (`POSIX_FADV_DONTNEED`) prior to each run.

### Shared Hardened Planes (Identical in Both Candidates)

1. **`dh-tms-format`**: 96-byte 32B-aligned checked file headers (magic, kind, format version, endian marker, `generation_id`, header CRC32, payload BLAKE3), single `CheckedMmap` validation boundary before typed `Pod` access, atomic generation publication (`generations/gen-<hex>/` + `CURRENT.json`), checked LEB128 varints, binary zero-JSON `MmapContentCatalog`.
2. **`dh-tms-grammar`**: Unicode NFKD + Latin-extended accent folding, exact-token release-tag parsing (fixing substring tag bugs, sequel-digit preservation, in-title year preservation, trailing-hyphen release-group handling, `S01E02-E04` range expansion, collision-free tagged `u64` per-entity/global episode keys), exact 34-keyword `xxx_strong` adult matcher + 23 media extensions + signed media-byte dominance across all files, exact `release-name-policy-v1`, exact `catalogueguard` privacy matchers.
3. **`dh-tms-hashdir`**: Exact 40-hex v1 / 64-hex v2 parsing (zero silent truncation), canonical deduplication, per-generation keyed XXH3 hashing (HashDoS mitigation), dense 20B reverse `doc_id → info_hash` column for direct magnet URI emission, 8B fingerprint Robin-Hood table + 512-bit split-block Bloom filter, tagged BEP-52 32B v2/hybrid alias tables (`44,616` real v2 aliases).
4. **`dh-tms-core`**: 32B cache-line-aligned `MetaRow`, 1B packed `facet_col.bin`, separate seekable Name column (`names_data.bin` + `names_offsets.bin`), byte-capped 16-record `256 KiB` corpus-trained Zstd-dictionary File-Tree blocks with per-block CRC32 and dedicated single-record overflow blocks for mega-torrents (`>256 KiB`, tested up to `80,000` files / `>4 MiB`).
5. **`dh-tms-policy`**: All 12 `serving.Policy` predicates, dense atomic deny bitset (`~50 ns` atomic revoke enforced at response egress to block in-flight readers holding older snapshots), per-doc atomic `u32` `payload_expires_epoch` checked on every read, `ArcSwap<PolicySnapshot>`, PG-LSN-fenced `GovernanceLease` fail-closed gate, generation-tagged CRC32-checksummed `fsync`'d `policy.wal` with torn-tail recovery (verified under OS `SIGKILL`), and `CsamOrDelete` physical-scrub tombstoning.
6. **`dh-tms-swarm`**: `MmapRaw`-backed 16-byte seqlock-coherent atomic slots (24-bit counts with unknown sentinel, monotonic `checked_epoch_min` rejecting stale scrapes, coherent `peak_seeders`), WAL-before-mutate `swarm.wal` with `fdatasync` + checkpointing + torn-tail recovery (verified under OS `SIGKILL`), and ring-rotated fair `SelectStale` (zero high-`doc_id` starvation).

### Candidates Compared

- **Candidate A — `DH-TMS Hybrid-Tantivy`**: Embedded Rust `tantivy` (`0.27.0`) zero-stored-fields index with multi-field BM25 (`title_stem`, `entity_title`, `raw_name`, `file_tokens`), trigram field, episode/fallback term fields, custom `'static` policy-aware collectors pushing `PolicyReaderHandle` into segment collection, and native `IndexWriter::commit` + `reader.reload()` for live updates.
- **Candidate B — `DH-TMS Custom v0.3`**: `(content_type, published_epoch DESC)` `doc_id` ordering (eliminating `time_desc.bin`), single unified unsaturated `u16` BM25F impact inverted index with `df=1` inline singletons and dense-term (`df >= 50K`) SIMD Roaring overlay, Block-Max WAND for disjunctive `OR` + block-max pruned conjunctive `AND` (exact `f64` score accumulation and deterministic `doc_id ASC` tie-breaking, property-tested against an exhaustive reference scorer), policy-bitmap block skipping (`mask.range_cardinality == 0`), rolling daily `created_day_bms` `dashstats`, trigram index, and an append-only durable L0 overlay WAL (`l0.wal`).

### Pre-Committed Gate 0 Decision Rule

- **Choose Candidate A (`Hybrid-Tantivy`)** if it passes 100% result parity, stays within **`≤ 1.5×`** of Candidate B's p99 latency (`< 15–20 ms` warm/loaded p99 at `1×` and `3×`), stays within **`≤ 1.35×`** of Candidate B's on-disk footprint, and fits cleanly inside `memory.peak < 6 GiB`.
- **Choose Candidate B (`Custom v0.3`)** if Tantivy cannot meet the `6 GiB` memory/footprint envelope, `serving.Policy` bitmap pushdown latency, or live reclassification throughput.

---

## 3. Verification & Parity Results (1× Real Governance vs PostgreSQL 18.6 Oracle)

- **Workspace Verification:** All 9 crates compiled in release mode with zero warnings and passed 100% of unit tests, `proptest` randomized BMW/OR tie-break parity checks, 80,000-file mega-torrent overflow checks, and both real OS `SIGKILL` crash-recovery tests (`dh-tms-policy` zero resurrection; `dh-tms-swarm` zero torn slots).
- **Full `10,824,399`-Torrent `serving.Policy` Audit (Verbatim Go SQL vs `DH-TMS`):**
  - **Visibility leaks:** **`0`** across `base_eligible`, `pub_eligible`, and `payload_eligible`.
  - **`pub_eligible`:** PG `300,039` vs `DH-TMS` `300,039` (**100.000000% exact match**, `0` diff).
  - **`payload_eligible` (snapshot epoch `2026-11-11T00:00:00Z`):** PG `60` vs `DH-TMS` `60` (**100.000000% exact match**, `0` diff).
  - **Individual predicates (`0` diff across all 10.82M rows):** `public`, `quarantined` (`5`), `privacy_tainted` (`3,085`), `excluded` (`754,146`), `held` (`4`), `name_blocked` (`2,042,831`), `pub_approved` (`300,411`).
  - **Audit discovery in existing PostgreSQL serving SQL (`internal/serving/policy.go:77`):** `strings.ReplaceAll(pattern, "\d", "0-9")` corrupts the `fc# ?ppv` adult keyword pattern into literal `[Ff][Cc]0-9` (missing `[0-9]` brackets), leaking `23,235` matching adult releases in PostgreSQL `base_eligible`. `DH-TMS` compiles `#` to `[0-9]` and blocks all `23,235` cleanly. (Tracked for an independent bug-fix PR in `internal/serving/policy.go`.)
- **Per-Query Infohash-Set Parity & 500-Query Fallback Recall Audit (Verbatim Go `matchesAnyResolvedContent` Oracle):**
  - **Visibility leaks across all evaluated queries:** **`0`** (both candidates).
  - **Linked ID & RSS parity:** `id_movie_base` **100.0%** (`4,332/4,332`), `id_movie_pub` **100.0%** (`565/565`), `id_tv_pub` **100.0%** recall (`573/573`), `rss_tv_pub` **100.0%** (`100/100`), `rss_tv_payload` **100.0%** (`35/35`).
  - **500-Query Unlinked Fallback Audit (`250` TV + `250` Movie):**
    - Candidate A (`Hybrid-Tantivy`): **`100.0%` (`72/72`)** recall (`28/28` TV, `44/44` Movie; plus `3,330` unclassified `CT_UNKNOWN` releases surfaced after typed matches).
    - Candidate B (`Custom v0.3`): **`100.0%` (`72/72`)** recall (`28/28` TV, `44/44` Movie; plus `3,329` unclassified `CT_UNKNOWN` releases surfaced after typed matches).
  - **Text-search file-token note:** On 100 `title_and_pub` queries, every `DH-TMS` match was 100% valid (`0` leaks); PostgreSQL `tsv` returned additional hits on multi-file pack torrents exceeding `12` files because Gate 0 capped `file_tokens` at 12 files per torrent. Phase 1 ingest will raise/align file-token indexing with PostgreSQL `tsv` coverage.

---

## 4. Measured Head-to-Head Benchmark Results (Strict `MemoryMax=6G, MemorySwapMax=0`)

All `DH-TMS` runs evicted host page cache (`POSIX_FADV_DONTNEED`) immediately before entering a fresh `6 GiB` cgroup v2 scope so `cgroup memory.peak` charges 100% of the active mapped working set honestly.

### 4.1 Storage Footprint & Build Time

| Scale | PG 18.6 (Fresh Zero-Bloat) | Candidate A (`Hybrid-Tantivy`) | Candidate B (`Custom v0.3`) |
|---|---|---|---|
| **1× (`10.82M` torrents, `133.4M` files)** | `72.34 GiB` | **`5.56 GiB`** (`3.37` shared + `2.19` search; search build `76.5s`) | **`5.74 GiB`** (`3.37` shared + `1.45` inv/struct + `0.92` trigrams; search build `333.0s`) |
| **3×\* (`32.47M` docs, `133.4M` files)** | *(>200 GiB)* | **`11.18 GiB`** (`6.49` shared + `4.69` search; search build `206.3s`) | **`11.99 GiB`** (`6.49` shared + `2.82` inv/struct + `2.69` trigrams; search build `490.0s`) |
| **3× Projected Full-Files (`400M` files)** | *(>350 GiB)* | `~14.8 GiB` (`+3.60 GiB` Zstd-dict `tree_blocks.bin`) | `~15.6 GiB` (`+3.60 GiB` Zstd-dict `tree_blocks.bin`) |

*\*Note: 3× replication follows the established 3× benchmark fixture (3× torrents/governance/search metadata, 1× file trees). Full 3× file-tree replication adds `2 × 1.80 GiB = +3.60 GiB` to `tree_blocks.bin` (`CorePlane` only; zero impact on search index working set or memory peak).*

### 4.2 Warm p99 Latency, Memory Peak, Concurrent Stress & Live Reclassification

| Workload / Metric | PG 18.6 (`1×`, `6 GiB`) | Cand. A (`1×`) | Cand. B (`1×`) | Ratio `A/B` (`1×`) | Cand. A (`3×`) | Cand. B (`3×`) | Ratio `A/B` (`3×`) |
|---|---|---|---|---|---|---|---|
| **Cgroup v2 `memory.peak` (warm+stress)** | `6.000 GiB` (cap) | `2.687 GiB` | **`2.292 GiB`** | `1.17×` | `5.537 GiB` (92%) | **`4.870 GiB`** | `1.14×` |
| **Cgroup v2 `memory.peak` (cold)** | `6.000 GiB` (cap) | `2.455 GiB` | **`2.116 GiB`** | `1.16×` | `4.974 GiB` | **`4.324 GiB`** | `1.15×` |
| **`title_and_pub` warm p99** | `3,842.5 ms` (1 T/O) | `9.42 ms` | **`0.96 ms`** | **`9.8×`** | **`25.65 ms` (FAIL)** | **`3.93 ms`** | **`6.5×`** |
| **`title_and_bm25_pub` warm p99** | `573.9 ms` (1 T/O) | — | — | — | — | — | — |
| **`title_and_base` warm p99** | — | `57.6 ms` | **`7.7 ms`** | **`7.5×`** | `51.1 ms` | **`40.0 ms`** | `1.3×` |
| **`title_or_pub` (Disj. OR) warm p99** | — | `71.23 ms` | **`34.80 ms`** | **`2.0×`** | `207.57 ms` | **`59.91 ms`** | **`3.5×`** |
| **`title_phrase_pub` warm p99** | — | `8.81 ms` | **`0.40 ms`** | **`22.1×`** | `13.2 ms` | **`0.98 ms`** | **`13.5×`** |
| **`torznab_tvsearch_pub` warm p99** | `404.1 ms` | `6.71 ms` | **`1.06 ms`** | **`6.3×`** | `16.37 ms` | **`1.88 ms`** | **`8.7×`** |
| **`facet_counts_pub` warm p99** | `3,771.0 ms` | `4.79 ms` | **`0.57 ms`** | **`8.4×`** | `11.71 ms` | **`1.18 ms`** | **`10.0×`** |
| **`id_tv_pub` warm p99** | `18.82 ms` | `2.42 ms` | **`0.030 ms`** | **`80.5×`** | `9.10 ms` | **`0.439 ms`** | **`20.7×`** |
| **`id_movie_pub` warm p99** | `12.82 ms` | `3.3 ms` | **`0.015 ms`** | **`220×`** | — | — | — |
| **`fallback_movie_base` warm p99** | — | `28.80 ms` | **`14.90 ms`** | **`1.9×`** | `54.48 ms` | **`40.10 ms`** | `1.4×` |
| **`prowlarr_tv_rss_pub` warm p99** | **`>15,000 ms` (10/10 T/O)** | **`58.71 ms` (FAIL)** | **`0.010 ms`** | **`5,871×`** | **`189.19 ms` (FAIL)** | **`0.015 ms`** | **`12,613×`** |
| **`dashstats_30d_pub` warm p99** | **`>15,000 ms` (5/5 T/O)** | **`37.29 ms` (FAIL)** | **`0.631 ms`** | **`59.1×`** | **`127.48 ms` (FAIL)** | **`2.906 ms`** | **`43.9×`** |
| **`substring_trigram_base` warm p99** | **`>15,000 ms` (5/10 T/O)** | `79.22 ms` | **`11.58 ms`** | **`6.8×`** | `216.81 ms` | **`26.66 ms`** | **`8.1×`** |
| **8-Thread Stress QPS** | — | `472.8 QPS` | **`53,199 QPS`** | **`112.5×`** | `153.0 QPS` | **`26,203 QPS`** | **`171.3×`** |
| **8-Thread Loaded `title_pub` p99** | — | `9.45 ms` | **`0.70 ms`** | **`13.6×`** | `15.86 ms` | **`1.31 ms`** | **`12.1×`** |
| **Live Reclass (`1,000` docs, `50`/batch)** | — | `251 docs/s` (`253 ms` p99) | **`7,586 docs/s`** (`6.4 ms` p99) | **`30.2×`** | `240 docs/s` (`326 ms` p99) | **`7,540 docs/s`** (`7.6 ms` p99) | **`31.4×`** |
| **Durable `fdatasync` Swarm WAL** | — | `50,398/s` | **`46,003–62,191/s`** | Tie | `36,492/s` | **`56,672/s`** | `1.55×` |
| **Emitted / Paused-Reader Leaks** | — | `0 / 0` | **`0 / 0`** | Tie | `0 / 0` | **`0 / 0`** | Tie |

---

## 5. Decision & Rationale

**Decision: Select Candidate B (`DH-TMS Custom v0.3`) with an Append-Only L0 Overlay + Periodic Offline Generation Rebuild & Atomic Swap (`CURRENT.json`).**

### Why Candidate A (`Hybrid-Tantivy`) Failed the Gate 0 Rule

1. **`serving.Policy` Bitmap Pushdown Architectural Mismatch:** In `DH-Waypoint`, consumer visibility (`pub_eligible` = `2.77%` of corpus; `payload_eligible` = `0.00055%`) is an external, rapidly mutated fail-closed bitset. Tantivy cannot push an external `RoaringBitmap` into its Block-Max WAND block skipper; it must decompress, score, and materialize fast-field `doc_id`s across 4 text fields for 100% of matching postings before discarding 97.2% inside the collector. Even if RSS, `dashstats`, and ID lookups were stripped out of Tantivy into custom bitmap planes, Tantivy still violates the `≤ 1.5×` latency criterion on pure full-text search (`6.5×–9.8×` slower on `title_and_pub`, hitting **`25.65 ms` > `20 ms`** at `3×`), `6.3×–10.0×` slower on `torznab_tvsearch_pub` and `facet_counts_pub`, and **`112×–171×` lower** concurrent QPS (`153 QPS` vs `26,203 QPS` at `3×`).
2. **Live Reclassification Throughput (`30×–31×` slower):** Reclassification commits on a 10M–32M doc Tantivy index incur segment flush + reader reload overhead (`240 docs/s`, `326 ms` p99 commit), whereas `Custom v0.3`'s append-only L0 WAL absorbs **`7,540 docs/s`** at **`7.55 ms` p99** with zero degradation from `1×` to `3×`.
3. **3× Cgroup Memory Envelope:** Under strict `6 GiB` cgroup v2 (`MemorySwapMax=0`), Candidate A climbs to **`5.537 GiB`** (`92.3%` of ceiling) at `3×`, leaving only `474 MiB` of margin before page thrashing/OOM, while Candidate B stays at **`4.870 GiB`** (`1.13 GiB` headroom).

### Eliminating Custom Multi-Level LSM Maintenance Debt

The principal risk flagged in adversarial reviews of a custom engine was maintaining a multi-level LSM compaction engine. Gate 0 benchmarks prove a **much simpler two-tier lifecycle** is sufficient and lower-risk:

1. **Immutable Base Generation (`generations/gen-<hex>/`)**: Rebuilt offline from PostgreSQL 18 snapshots (`~8–10 minutes` at `1×`, `~14 minutes` at `3×`) and published atomically via `CURRENT.json` + generation-tagged headers.
2. **Bounded Append-Only L0 Overlay (`l0.wal`)**: Absorbs live inserts and reclassifications (`~7,500 docs/s`, `~7 ms` p99 commit) plus `policy.wal` revocations (`~50 ns` atomic deny bitset + `fdatasync`) and `swarm.wal` telemetry (`~50K–62K` durable updates/s) between scheduled offline generation rebuilds (e.g., nightly or when L0 reaches a bounded threshold such as `250K` deltas).

This captures 100% of Candidate B's measured performance, memory, and policy-pushdown advantages without writing or maintaining multi-level background LSM compaction.

---

## 6. Consequences & Phased Rollout Guardrails

- **Phase 1 (Core Rust Crates + `dh-tmsd` + Go Client `internal/database/dhtms/`):**
  - Integrate MIT Rust workspace + Unix-socket/HTTP `dh-tmsd` daemon + Go client behind a clean interface preserving `scripts/check_processor_boundary.py`.
  - Align `file_tokens` indexing coverage with PostgreSQL `tsv` so multi-file pack queries (`>12` files) achieve full text recall parity alongside the already verified `100.0%` fallback, ID, RSS, and policy parity.
  - Keep unit, `proptest`, fuzz, and OS `SIGKILL` crash-recovery tests mandatory in CI.
- **Phase 2 (Shadow Mode & PG-LSN CDC Soak):** Run `DH-TMS` in shadow behind PG 18.6 with PG-LSN lease fencing (`GovernanceLease`), continuous parity sampling, and zero-leak assertions for ≥ 7 days before enabling consumer reads.
- **Phase 3 (Feature-Flagged Read Cutover):** Gate all consumer read paths behind `DH_TMS_READ_MODE=off|shadow|serve` with instant rollback to PostgreSQL 18.
- **Phase 4 (Swarm Telemetry Cutover & PG Index Diet):** Cut high-churn `torrent_tracker_seeds` writes onto `dh-tms-swarm` WAL/checkpoint plane and drop redundant heavy search/trigram indexes from PostgreSQL only after sustained production soak.
