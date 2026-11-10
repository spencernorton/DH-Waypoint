# CSAM defense

> **Status:** Implemented and shipping. This document is the operator + reviewer guide; design rationale lives in `internal/csamblocklist/doc.go` in the dh-waypoint core repo.

## The problem

A standard DHT crawler discovers infohashes via DHT `get_peers` / `announce_peer` traffic, then fetches the torrent's metadata via the BEP-9 extension protocol over a real TCP/uTP connection to a peer claiming to seed the content. Only after the metadata fetch can the classifier inspect the title and file paths and decide to drop the torrent.

That post-fetch decision is too late for one specific concern. Peer-tracking services (e.g. "I know what you download") record every IP they see participating in a swarm. By the time the classifier has the title, the operator's IP has already been seen connecting to the swarm. For most content this is fine — your IP appears in countless swarms — but for **CSAM** it is not acceptable under any operational tradeoff.

The community discussion (originally [bitmagnet-io/bitmagnet#494](https://github.com/bitmagnet-io/bitmagnet/issues/494)) settled on a **double-hashed public blocklist** as the only credible defense:

- A public list of plaintext CSAM infohashes would itself be a directory of CSAM. Distributing such a list is harmful.
- A public list of **SHA-256-of-(SHA-1 infohash)** is one-way: receivers can compute the same double-hash on every infohash they discover and reject matches; nobody can run the list backwards to find content.

This is the defense DH-Waypoint implements.

## Layered defense

In execution order on every newly-discovered infohash:

```
DHT discovery
    │
    ▼
[1] csamblocklist.Manager.Filter ─── community-feed double-hashes
    │   and this instance's own banned-keyword matches (see [3a])
    │   reject pre-fetch
    │
    ▼
[2] BlockingManager.Filter ─── this instance's own observations
    │   reject pre-fetch
    │
    ▼
[3] BEP-9 metadata fetch ─── the swarm-touching network call
    │   (defense-in-depth: csamblocklist.IsBlocked re-checks here
    │   in case a feed refresh introduced the hash after triage)
    │
    ▼
[3a] Built-in banned-keyword check ─── every PROCESSING_MODE
    │   the fetched name and file paths are tested against the built-in
    │   `keywords.banned` list before anything is stored. A match is
    │   dropped, its double-hash is saved in `csam_local_observations`
    │   and exported as in [5], and [1] rejects the hash from then on,
    │   across restarts. The import API and the quarantine restore
    │   apply the same check before they write a torrent.
    │
    ▼
[3b] Processor check ─── every PROCESSING_MODE, reprocess requests too
    │   a stored torrent whose name or file paths match the built-in
    │   list is removed before the classifier or an optional processor
    │   sees it, and remembered and exported as in [3a]. See "Removing
    │   a stored match".
    │
    ▼
[4] CEL classifier ─── post-fetch, sees the title + files
    │   (PROCESSING_MODE=full only)
    │   `keywords.banned` regex → ErrDeleteTorrent. [3b] has already
    │   removed every match of the built-in list, so this acts on
    │   keywords an operator added in a classifier file; the delete and
    │   any BlockingManager.Block are held while catalogue removal is
    │   disabled, so nothing is removed or added to [2]
    │
    ▼
[5] csamblocklist.Exporter.Record ─── re-checks banned-keyword regex,
        appends double-hash to local JSONL log,
        optional POST to community upstream
        (closes the loop: this instance's observation
         is published as a community-feed seed, so other
         instances catch the hash at [1] going forward)
```

## Removing a stored match

The processor tests every stored torrent it loads against the built-in list. This has no switch. The sweep described below hands its matches to the same removal. The stored row is locked and tested again, so a row that was renamed in the meantime is left alone. A match is then removed in one transaction:

- **Deleted:** the torrent row and the tables that follow it (files, contents, hints, tags, sources, pieces, tracker seeds), and every other row that can hold its name or a file path: processor jobs with their attempts, applications, exclusions, feedback events and reprocess requests; junk-purge judgments, quarantine entries and batch items; evaluation captures and the retired LLM queue; the frozen evaluation set, the shadow-policy log, the private file list and the field-repair journal.
- **Blanked:** verdict events for the hash lose `reason` and `evidence`. A catalogue-recovery snapshot of it loses its copy and reason, and can no longer be restored. A junk-purge batch input file that contains it is emptied, because one file holds the names of the whole run.
- **Kept:** one verdict (`tombstoned`, mechanism `csam`, no reason, no evidence), the double hash in `csam_local_observations`, and tables that hold no name or path (liveness, tracker statistics, receipts, publication state).

No copy is made and there is no undo. The removal does not go through catalogue recovery and is not held while catalogue removal is disabled. A torrent with a v1 and a v2 hash is removed under both.

**Protected torrents are hidden, not deleted.** A match with the private flag, a canonical label, *arr or qBittorrent evidence, a grab attempt, a hint with a content id or a protected tag is tied to the operator's own tools and is most likely a false hit. It gets a publication hold with the reason `banned_keyword`: no search or Torznab feed serves it and no optional processor receives it. A private torrent takes no hold, because it is served nowhere already. Evidence stored under the 20-byte form of a v2 hash, which is how qBittorrent and the *arr tools know a v2-only torrent, protects it too. The removal does not remember or export a hidden match. Releasing the hold lasts until the processor loads the torrent again.

Only the built-in list removes. Keywords an operator adds in a classifier file never delete this way. A catalogue-recovery restore refuses a snapshot that matches.

## Cleaning up what is already stored

The checks above act on a torrent when it arrives or when the processor loads it. A sweep reads everything that was stored before them, once. It runs in the background with the `csamblocklist` worker, which `worker run --all` includes, and uses the matcher of the live check.

`CSAM_SWEEP_MODE` sets what it does with a match:

| Value | What happens |
|---|---|
| `apply` (default) | Each match goes to the removal above: deleted with no copy, or hidden when it is tied to your own tools. This cannot be undone. |
| `dry_run` | Matches are counted. Nothing is changed. |
| `off` | Nothing is read. |

Any other value stops the start.

To see the numbers before the sweep deletes anything, start with `CSAM_SWEEP_MODE=dry_run`, wait until `sweep_done` is 1, read `sweep_matches` from `/metrics`, then set `apply` and restart. A new mode is a new pass from the first row.

The setting covers the sweep only. In every mode, `dry_run` and `off` too, the processor removes a stored match when it loads one. Give every worker process the same mode: a process with another mode starts the pass again from the first row.

What it reads:

- `torrents`, with the file paths in `torrent_files`.
- Copies whose torrent row is gone: junk-purge quarantine entries (name and file list), junk-purge judgments and batch items (name), catalogue-recovery snapshots (name and file list), the name that earlier releases sampled into the evidence of a classifier-delete verdict, and the frozen evaluation set (name).

Every row of a side table is read. A row is tested only when no stored torrent has its hash, because removing a stored torrent cleans its side rows. A match found there is removed by its hash, together with every other row for that hash. When that hash is tied to your own tools the copy is kept as it is and counted as `hidden_protected`: there is no torrent to hold, and nothing serves the copy.

The position and the counts are saved after every batch of 1,000 rows, in the `key_values` table under the key `csam_sweep`. A restart continues where the last batch ended, and `/metrics` shows the whole pass. Only one sweep runs at a time, also with several worker processes. After each batch it rests as long as the batch took.

When a removal fails, for example because it cannot get its locks within 2 seconds, the sweep waits 30 seconds and tries the same row again. It never skips a match. `sweep_done` stays 0 meanwhile, `sweep_batch_errors_total` counts each failed batch and the log names the table.

The pass runs once for each list and mode. A release that changes the built-in list runs it again.

## Honest limits

- **The sweep reads names where they are stored as names.** For a torrent that is gone it does not read the tables that hold only processor payloads or derived rows: jobs, applications, exclusions, feedback events, reprocess requests, the retired LLM queue, evaluation captures, the field-repair journal, the name-policy shadow and the private file list. It reaches those rows through another copy. The junk purge kept a quarantine entry and catalogue recovery keeps a snapshot, and a match in either removes every row for the hash. Such a row is missed only when its torrent row was deleted some other way and no such copy is left.
- **A torrent that is gone is protected by less.** Its private flag, tags and hints went with the torrent row. Only a canonical label, *arr or qBittorrent evidence or a grab attempt keeps its copies.
- **In a judgment, a batch item and a verdict sample the sweep tests the name only.** Those tables hold no file paths. The judge's free-text reason is not tested.
- **A dry run counts rows.** A torrent that left a copy in several tables counts once in each. `apply` removes every copy at the first one.
- **A hidden match is not fully cut off.** Feedback events and tasks of the retired LLM queue that were queued before the hold are still delivered. If the crawler fetches the hash again, which it does for a stored row without file information, the check before storage ([3a]) remembers and exports it like any other match.
- **Copies outside the database are not reached.** A name already delivered to an optional processor, or written to a log or a backup before the removal, stays there.
- **First observation across the network** still incurs one BEP-9 fetch per network. The defense converges as observations propagate into community feeds — it does not eliminate.
- **Bloom filter false positives** can reject a non-CSAM infohash that collides with a feed entry. With the default FPR of `0.001` and feeds in the 1k–100k range, the practical FP rate is very low. An FP causes one infohash to never be indexed by this instance — not a safety issue, just a curation miss. Operators can lower FPR at the cost of memory.
- **Compromised feeds** could publish hashes that aren't actually CSAM. Operators are responsible for the trust they place in any feed URL they configure. The package exposes `feed_refresh_total{outcome}` and per-feed `feed_entries` metrics so an operator can spot a feed flooding the blocklist.

## Wire format

Feed body is plain text, one entry per line:

```
# This is a comment line.
# Anything after `#` is ignored to end-of-line.

de47c9b27eb8d300dbb5f2c353e632c393262cf06340c4fa7f1b40c4cbd36f90
0a1b2c3d...                       (64 lowercase hex chars)
```

- One double-hash per line, `64` lowercase hex chars (SHA-256 = 256 bits).
- Comment lines start with `#` (after optional whitespace).
- Blank lines are skipped.
- Non-conforming lines are counted as parse errors but do not abort ingest of the rest of the feed.
- Maximum body size defaults to 64 MiB (~1M entries); configurable via `CSAM_BLOCKLIST_FEED_MAX_BYTES`.

The double-hash function is **SHA-256 of the raw 20-byte SHA-1 infohash bytes** — not the hex-encoded string. Feeds using a different hash function will not match.

## Operator config

`CSAM_SWEEP_MODE` (`apply`, `dry_run` or `off`; default `apply`) is described under "Cleaning up what is already stored". All other variables are env-prefix `CSAM_BLOCKLIST_*`:

| Variable | Default | Meaning |
|---|---|---|
| `ENABLED` | `true` | Switches the outside feeds. `false` fetches none. The built-in banned-keyword check has no switch. |
| `FEED_URLS` | *(empty)* | Comma/space-separated feed URLs. Empty = no feed is fetched. |
| `FEED_REFRESH_INTERVAL` | `6h` | How often to re-poll all feeds. |
| `FEED_FETCH_TIMEOUT` | `30s` | Per-feed HTTP timeout. |
| `FEED_MAX_BYTES` | `67108864` | Per-feed body cap (64 MiB). |
| `BLOOM_CAPACITY` | `1000000` | Bloom filter capacity. |
| `BLOOM_FALSE_POSITIVE_RATE` | `0.001` | Target FPR. |
| `EXPORT_ENABLED` | `true` | Self-export of confirmed observations. |
| `EXPORT_FILE_PATH` | `data/csam-double-hashes.jsonl` | Local JSONL log path. |
| `EXPORT_UPSTREAM_URL` | *(empty)* | Optional outbound POST endpoint. |
| `EXPORT_UPSTREAM_AUTH_HEADER` | *(empty)* | Full `Authorization` header value. |
| `EXPORT_UPSTREAM_TIMEOUT` | `10s` | Per-POST timeout. |

### Recommended starting profile

- `CSAM_BLOCKLIST_ENABLED=true` (default)
- `CSAM_BLOCKLIST_FEED_URLS=` — opt into a feed when one becomes available you trust
- `CSAM_BLOCKLIST_EXPORT_ENABLED=true` (default) — local log only
- `CSAM_BLOCKLIST_EXPORT_UPSTREAM_URL=` — leave empty until a community collection endpoint exists you want to contribute to

## Metrics

All under namespace `dh_waypoint_csam_blocklist_`:

- `prefetch_blocks_total` — counter. Hashes rejected pre-fetch.
- `lookups_total` — counter. Total hot-path lookups.
- `feed_refresh_total{feed,outcome}` — counter. `outcome=success|error`.
- `feed_refresh_duration_seconds{feed}` — histogram.
- `feed_entries{feed}` — gauge. Per-feed entry count.
- `entries` — gauge. Total entries across all feeds.
- `keyword_matches_total{source}` — counter. Matches dropped before storage. `source=crawler|import`.
- `stored_matches_total{source,outcome}` — counter. Stored matches handled. `source=processor|sweep`, `outcome=removed|hidden_protected`.
- `sweep_rows_scanned{table}` — gauge. Rows the sweep has read in this pass. In a side table every row is read and only a row whose torrent is gone is tested.
- `sweep_matches{table,outcome}` — gauge. Matches the sweep found in this pass. `outcome=removed|hidden_protected` with `apply`, `would_remove|would_hide_protected` with `dry_run`.
- `sweep_done` — gauge. 1 once the pass is complete, 0 before that and with the sweep off.
- `sweep_batch_errors_total` — counter. Sweep batches that failed and are tried again. It keeps rising while the sweep is stuck on one row.
- `export_total{outcome}` — counter. `outcome=local_ok|local_err|upstream_ok|upstream_err|skipped_no_match`.

See [reference/metrics.md](reference/metrics.md) for the catalogue context.

## Self-export observation log

When a torrent matches the built-in banned-keyword list, before storage ([3a]) or as a stored row the processor removes ([3b]), one JSONL line is appended to `EXPORT_FILE_PATH`:

```json
{"ts":"2026-04-27T03:14:15.926Z","double_hash":"de47c9b2...","reason":"banned_keyword"}
```

The raw infohash is never written. The title and file paths are never written. The log itself is non-useful as a CSAM directory if it leaks: it is the same shape as a public feed.

If `EXPORT_UPSTREAM_URL` is set, the same JSON object is POSTed to the configured endpoint with `Content-Type: application/json` and the optional `Authorization` header. POST failures are counted + logged but do not affect the local append.

## Testing the wiring

A minimal end-to-end smoke test (operator-side):

```bash
# Stand up a one-shot feed
mkdir -p /tmp/csamfeed
cat > /tmp/csamfeed/feed.txt <<EOF
# test feed
de47c9b27eb8d300dbb5f2c353e632c393262cf06340c4fa7f1b40c4cbd36f90
EOF
python3 -m http.server -d /tmp/csamfeed 18080 &

# Configure dh-waypoint
CSAM_BLOCKLIST_FEED_URLS="http://localhost:18080/feed.txt" \
  ./dh-waypoint worker run --all

# Watch metrics
curl -s localhost:3333/metrics | grep csam_blocklist
```

The operator should see `feed_entries{feed="localhost:18080/feed.txt"}=1` within a refresh interval.

## Relationship to upstream

The original issue is open at `bitmagnet-io/bitmagnet#494`. As of the DH-Waypoint fork (2026-04-24), there is no upstream movement on the proposal. This is DH-Waypoint's independent implementation. If upstream later ships a compatible mechanism, the wire format is intentionally simple (plain-text lowercase hex, one per line) so feed compatibility is likely.

## See also

- [Configuration](configuration.md) — `CSAM_BLOCKLIST_*` env vars
- [Operations / Security](operations/security.md)
- [Reference / Metrics](reference/metrics.md)
- `SECURITY.md` at the repo root — supported versions + threat model
