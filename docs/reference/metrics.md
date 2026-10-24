# Prometheus metrics

Version 5 retires the embedded inference families. The current
optional-processor names, populations, reset and unknown-value semantics are in
[processor operator API](../processors/operator-api.md). Those families use only
`dh_waypoint_processors_*`.

DH-Waypoint exposes raw Prometheus metrics on the HTTP port (`3333`) at `/metrics`. Setting `HTTP_SERVER_API_KEY` gates it: scrapes must then send the key as `Authorization: Bearer` (Prometheus `authorization.credentials`) or `X-API-Key`. `/status` stays public. Without the key `/metrics` is open; for public exposure, place a reverse proxy in front. A scrape interval of `30s` is recommended — it balances telemetry granularity with backend overhead.

The metric namespace was migrated from `bitmagnet_*` to `dh_waypoint_*` on 2026-04-24, and the legacy `bitmagnet_*` dual-emit was switched off on 2026-09-02. Only the process collector keeps the old prefix (`bitmagnet_process_*`); Go runtime metrics are `go_*`.

This page covers the main families. The live `/metrics` output is the complete list.

## Scrape configuration

Add this job to your Prometheus `scrape_configs`:

```yaml
- job_name: dh-waypoint
  metrics_path: /metrics
  scrape_interval: 30s
  scrape_timeout: 15s
  static_configs:
    - targets: ['localhost:3333']
      labels:
        instance: main
        service: dh-waypoint
```

Replace `localhost:3333` with the Tailscale IP / service hostname when scraping from a different host.

## `dh_waypoint_dht_*` — DHT crawler

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_dht_crawler_persisted_total` | counter | `entity` | Cumulative entities (`Torrent`, `TorrentsTorrentSource`) successfully persisted via the DHT crawl path. |
| `dh_waypoint_dht_ktable_hashes_added` | counter | — | Cumulative infohashes added to the Kademlia routing table — the headline crawler-pulse metric. No `_total` suffix. |
| `dh_waypoint_dht_client_request_concurrency` | gauge | `query` | Outbound DHT requests in flight. |
| `dh_waypoint_dht_client_request_duration_seconds_count` | counter | `query` | Successful outbound DHT requests (histogram count companion). |
| `dh_waypoint_dht_client_request_duration_seconds_bucket` | histogram | `query`, `le` | Cumulative bucketed durations of successful requests. |

## `dh_waypoint_postgres_*` — pgstats collector

Scrape-driven, 5-second per-scrape timeout.

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_postgres_database_size_bytes` | gauge | — | Total on-disk size of the database. |
| `dh_waypoint_postgres_table_size_bytes` | gauge | `table` | Per-table on-disk size. Common labels: `torrents`, `torrent_contents`, `label_evidence`, `torrent_canonical_labels`. |
| `dh_waypoint_postgres_table_rows_estimate` | gauge | `table` | `pg_class.reltuples` row estimate per table. |
| `dh_waypoint_postgres_table_dead_tuples` | gauge | `table` | Dead tuples awaiting autovacuum. |
| `dh_waypoint_postgres_table_live_tuples` | gauge | `table` | Live tuples per table. |
| `dh_waypoint_postgres_table_last_autovacuum_age_seconds` | gauge | `table` | Seconds since the last autovacuum on this table. |
| `dh_waypoint_postgres_table_last_analyze_age_seconds` | gauge | `table` | Seconds since the last `ANALYZE` on this table. |
| `dh_waypoint_postgres_connections` | gauge | `state` | Server connections to this database from `pg_stat_activity`, grouped by state. |
| `dh_waypoint_postgres_pool_acquired_connections` | gauge | — | Connections checked out of the pgx pool. |
| `dh_waypoint_postgres_pool_idle_connections` | gauge | — | Idle connections held by the pgx pool. |
| `dh_waypoint_postgres_pool_max_connections` | gauge | — | Configured maximum size of the pgx pool. |

## `dh_waypoint_evidence_*` — `*arr` evidence ingest

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_evidence_events_persisted_total` | counter | `source`, `kind` | Successful evidence persists. `source` is the reporting client (`sonarr`, `radarr`, `readarr`, `lidarr`, `qbittorrent`). |
| `dh_waypoint_evidence_events_duplicated_total` | counter | `source`, `kind` | Events deduplicated against existing rows. |
| `dh_waypoint_evidence_events_rejected_total` | counter | `source`, `kind`, `reason` | Events rejected (auth, parse, schema). |
| `dh_waypoint_evidence_source_errors_total` | counter | `source`, `instance`, `stage` | Error count per source/stage; dashboard alert candidate. |
| `dh_waypoint_evidence_source_poll_duration_seconds` | histogram | `source`, `instance` | Poll latency distribution per source. |

## `dh_waypoint_classifier_*` — base classifier

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_classifier_deleted_total` | counter | `content_type`, `rule` | Torrents deleted by a classifier workflow rule: the content type assigned before the delete (`unknown` for the pre-parse banned-keyword rule) and the dotted path of the rule that fired. Counts completed removals only. |
| `dh_waypoint_classifier_delete_held_total` | counter | `content_type`, `rule` | Classifier deletes held because catalogue recovery is disabled: nothing was removed and the job completed. |

## `dh_waypoint_classifier_preempt_*` — canonical-label preempt

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_classifier_preempt_preempted_total` | counter | `source`, `media_type` | Runs skipped outright because the torrent's own \*arr label decided it (non-video types). |
| `dh_waypoint_classifier_preempt_constrained_total` | counter | `source`, `media_type` | Movie/TV runs whose type and identity hint were fixed by the torrent's own label before normal enrichment. |
| `dh_waypoint_classifier_preempt_misses_total` | counter | — | Runs with no label of their own. The expected steady state for freshly crawled torrents. |
| `dh_waypoint_classifier_preempt_lookup_errors_total` | counter | — | Label lookups that failed; the classifier ran without evidence. |
| `dh_waypoint_classifier_preempt_unknown_media_type_total` | counter | `source` | Labels whose media type cannot short-circuit anything; the classifier ran. |
| `dh_waypoint_classifier_preempt_evidence_title_total` | counter | `outcome` | Title-level \*arr evidence for torrents with no label of their own (`CLASSIFIER_EVIDENCE_TITLE_IDENTITY`). `applied`: the identity the \*arrs agree on for this title was hinted. `weak`: fewer than 2 other labels, or no ⅔ majority. `no_preference`: no other torrent with this title is labelled. `no_key`: the name did not parse to a title. `no_index`: labels could not be loaded. Absent while the flag is off. |

## `dh_waypoint_retention_*` — retention pipeline

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_retention_candidates_total` | counter | — | Torrents matched by the retention predicate before any cap. |
| `dh_waypoint_retention_would_purge_total` | counter | — | Torrents that *would* be purged (dry-run signal). |
| `dh_waypoint_retention_purged_total` | counter | — | Torrents actually deleted (only nonzero when `RETENTION_ENABLE_PURGE=true`). |
| `dh_waypoint_retention_cycle_errors_total` | counter | `stage` | Cycle errors by stage (`query`, `delete`). |
| `dh_waypoint_retention_cycle_duration_seconds` | histogram | — | Per-cycle pipeline latency. |

## `dh_waypoint_csam_blocklist_*` — CSAM defense

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_csam_blocklist_prefetch_blocks_total` | counter | — | Pre-fetch rejects (the headline defense win — these never touched a swarm). |
| `dh_waypoint_csam_blocklist_lookups_total` | counter | — | Lookups against the bloom filter. |
| `dh_waypoint_csam_blocklist_feed_refresh_total` | counter | `feed`, `outcome` | Per-feed refresh outcomes. |
| `dh_waypoint_csam_blocklist_feed_refresh_duration_seconds` | histogram | `feed` | Per-feed refresh latency. |
| `dh_waypoint_csam_blocklist_feed_entries` | gauge | `feed` | Active entries from each feed. |
| `dh_waypoint_csam_blocklist_entries` | gauge | — | Total bloom filter entries (sum of all feeds, deduplicated). |
| `dh_waypoint_csam_blocklist_export_total` | counter | `outcome` | Per-export outcomes (local JSONL append + optional upstream POST). |

See [csam-defense.md](../csam-defense.md) for the full architecture.

## `dh_waypoint_liveness_*` — torrent liveness

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_liveness_observations_total` | counter | `class`, `outcome` | Liveness observations dispatched to the resolver. `class` ∈ {`alive`, `suspect`}; `outcome` ∈ {`upsert`, `error`, `skip_private`, `blacklisted`, `dht_recovered`}. |
| `dh_waypoint_liveness_revalidations_total` | counter | `outcome` | DHT revalidation attempts on dead infohashes. `outcome` ∈ {`alive_again`, `still_dead`, `error`}. |
| `dh_waypoint_liveness_blacklist_size` | gauge | — | Current count of `torrent_liveness` rows with status `dead`, sampled by the revalidator. |
| `dh_waypoint_liveness_torznab_excluded_total` | counter | — | Torznab response items filtered out for liveness. |

## `dh_waypoint_quarantine_*` — quarantine maintenance

Active when `JUNKPURGE_MAINTENANCE_ENABLED=true`.

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_quarantine_expired_total` | counter | — | Quarantined torrents past `JUNKPURGE_QUARANTINE_DAYS`, tombstoned with their snapshot retained. |
| `dh_waypoint_quarantine_errors_total` | counter | `stage` | Maintenance errors (`expire`). |

## `dh_waypoint_contentfilter_*` — content filter

Active when `CONTENT_FILTER_ENABLED=true`. With `CONTENT_FILTER_ENFORCE=true` drop decisions count in `drop_total`, but the removal itself is held while [catalogue removal](../OPERATIONS.md#catalogue-removal) is disabled; otherwise they count in `would_drop_total`.

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `dh_waypoint_contentfilter_examined_total` | counter | — | Torrents evaluated by the filter. |
| `dh_waypoint_contentfilter_drop_total` | counter | `reason` | Enforced drop decisions by reason (`blocked_extension`, `non_english_language`, `non_latin_script`, `nsfw_keyword`, etc.). |
| `dh_waypoint_contentfilter_would_drop_total` | counter | `reason` | Drops the filter would make while enforcement is off. |

## Recommended dashboard panels

- **Crawl health** — `rate(dh_waypoint_dht_ktable_hashes_added[5m])`
- **Active DHT requests** — `sum(dh_waypoint_dht_client_request_concurrency)`
- **DB size** — `dh_waypoint_postgres_database_size_bytes`, `dh_waypoint_postgres_table_size_bytes{table="torrents"}`, `...{table="torrent_contents"}`, `...{table="label_evidence"}`, `...{table="torrent_canonical_labels"}`
- **Evidence flow** — `rate(dh_waypoint_evidence_events_persisted_total[5m])` by `source`, alongside `events_duplicated_total` + `events_rejected_total`
- **Canonical coverage** — `sum(dh_waypoint_postgres_table_rows_estimate{table="torrent_canonical_labels"}) / sum(dh_waypoint_postgres_table_rows_estimate{table="torrents"})` — what fraction of indexed torrents have a ground-truth label
- **Autovacuum health** — `dh_waypoint_postgres_table_last_autovacuum_age_seconds` — alert if `> 86400` on a high-churn table
- **Dead tuple pressure** — `dh_waypoint_postgres_table_dead_tuples / dh_waypoint_postgres_table_live_tuples`
- **External source health** — `rate(dh_waypoint_evidence_source_errors_total[5m])` by `source`, `stage`
- **Poll latency p95** — `histogram_quantile(0.95, sum by (le) (rate(dh_waypoint_evidence_source_poll_duration_seconds_bucket[5m])))`

## Cardinality notes

`table` on `dh_waypoint_postgres_*` metrics has one value per table in the schema, and `instance` on `dh_waypoint_evidence_*` one per configured \*arr or qBittorrent instance. All other labels on this page are enumerated (e.g., `outcome`, `class`, `source`, `stage`, `reason`) and have small bounded cardinality.

`feed` labels on `dh_waypoint_csam_blocklist_*` metrics scale with the number of configured CSAM feeds — typically ≤ 5. Not a cardinality concern.

## Grafana dashboard

A pre-configured Grafana dashboard ships in this repository at `observability/grafana-dashboards/dh-waypoint.json`. Import the JSON directly into your Grafana instance and point it at your Prometheus datasource. It covers DHT, metadata-request, crawler and queue activity; it does not include the panels above.

## See also

- [operations/monitoring.md](../operations/monitoring.md)
- [configuration.md](../configuration.md)
- [examples/README.md](../../examples/README.md)
- [csam-defense.md](../csam-defense.md)
