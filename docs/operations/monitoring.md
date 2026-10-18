# Monitoring

DH-Waypoint ships Prometheus metrics and a pre-built Grafana dashboard. This page walks through wiring them up and the alerts worth setting.

For the full metrics catalog see [reference/metrics.md](../reference/metrics.md).

## What you'll set up

1. Prometheus scraping DH-Waypoint's `/metrics` endpoint.
2. Grafana with the bundled DH-Waypoint dashboard imported.
3. (Optional) Alertmanager rules for the obvious failure modes.

## Prerequisites

- A running DH-Waypoint instance (port `3333` reachable from your Prometheus host).
- Prometheus 2.x or later.
- Grafana 10.x or later.

## Step 1 — Add the Prometheus scrape job

Append this to `scrape_configs:` in your `prometheus.yml`:

```yaml
- job_name: dh-waypoint
  metrics_path: /metrics
  scrape_interval: 30s
  scrape_timeout: 15s
  static_configs:
    - targets: ['dh-waypoint:3333']     # or 'localhost:3333' if Prometheus is local
      labels:
        instance: production
        service: dh-waypoint
```

Reload Prometheus (`SIGHUP` or `curl -X POST http://prometheus:9090/-/reload` if `--web.enable-lifecycle` is set).

## Step 2 — Verify the scrape

In the Prometheus UI:

- **Status → Targets** — `dh-waypoint` job should be `UP`.
- **Graph** — `up{job="dh-waypoint"}` returns `1`.
- **Graph** — `rate(dh_waypoint_dht_ktable_hashes_added_total[5m])` returns a positive rate after the crawler has been running ~3 minutes.

If `up == 0`, check from the Prometheus host:

```bash
curl -sS http://dh-waypoint:3333/metrics | head
```

Expect a few hundred lines of metric text. If the curl fails, it's network connectivity, not Prometheus.

## Step 3 — Import the Grafana dashboard

The dh-waypoint core repo ships a Grafana dashboard at `observability/grafana-dashboards/dh-waypoint.json`.

1. Grab the JSON file (clone the repo or download raw).
2. Grafana → **Dashboards → New → Import**.
3. Upload the JSON or paste it.
4. Pick your Prometheus data source.
5. Save.

The dashboard has panels for crawl health, DB size growth, evidence flow, classifier-preempt hit ratio, autovacuum lag, dead-tuple pressure, and more. It defaults to a 6-hour window — bump to 24h for trend visibility.

## Step 4 — Key panels to watch

Even without the dashboard, these are the queries that matter:

| What | Query | Healthy range |
|---|---|---|
| Crawl pulse | `rate(dh_waypoint_dht_ktable_hashes_added_total[5m])` | nonzero, climbing |
| DHT request concurrency | `sum(dh_waypoint_dht_client_request_concurrency)` | > 100 in steady state |
| Persisted torrents/sec | `rate(dh_waypoint_dht_crawler_persisted_total[5m])` | proportional to scaling factor |
| DB total size | `dh_waypoint_postgres_database_size_bytes` | grows ~5–15 GB/month |
| Evidence flow by source | `rate(dh_waypoint_evidence_events_persisted_total[5m]) by (source)` | nonzero on at least one source |
| Canonical coverage | `sum(dh_waypoint_postgres_table_rows_estimate{table="torrent_canonical_labels"}) / sum(dh_waypoint_postgres_table_rows_estimate{table="torrents"})` | climbs from 0 toward ~0.2–0.3 over weeks |
| Autovacuum age (torrents) | `dh_waypoint_postgres_table_last_autovacuum_age_seconds{table="torrents"}` | < 86400 (24 h) |
| Dead-tuple pressure | `dh_waypoint_postgres_table_dead_tuples / dh_waypoint_postgres_table_live_tuples` | < 0.2 |
| Preempt hit ratio | `rate(dh_waypoint_classifier_preempt_lookup_total{result="hit"}[10m]) / rate(dh_waypoint_classifier_preempt_lookup_total[10m])` | grows over weeks; early deployments 0% |
| LLM cache hit ratio (if enabled) | `rate(dh_waypoint_classifier_llm_cache_hits_total[10m]) / (rate(dh_waypoint_classifier_llm_cache_hits_total[10m]) + rate(dh_waypoint_classifier_llm_cache_misses_total[10m]))` | > 0.5 |

## Step 5 — Recommended alerts

A minimal Alertmanager-rule set for production:

```yaml
groups:
  - name: dh-waypoint
    rules:
      - alert: DH-WaypointDHTStalled
        expr: rate(dh_waypoint_dht_ktable_hashes_added_total[10m]) == 0
        for: 15m
        annotations:
          summary: "DH-Waypoint DHT crawl has stopped (no new hashes for 15 min)"

      - alert: DH-WaypointAutovacuumLagging
        expr: dh_waypoint_postgres_table_last_autovacuum_age_seconds{table="torrents"} > 86400
        for: 30m
        annotations:
          summary: "Postgres autovacuum on torrents table is > 24h stale"

      - alert: DH-WaypointEvidenceErrors
        expr: rate(dh_waypoint_evidence_source_errors_total[5m]) > 0.1
        for: 10m
        annotations:
          summary: "Evidence ingest error rate elevated"

      - alert: DH-WaypointLLMErrorRate
        expr: |
          rate(dh_waypoint_classifier_llm_invocations_total{result="error"}[10m])
          / rate(dh_waypoint_classifier_llm_invocations_total[10m])
          > 0.05
        for: 10m
        annotations:
          summary: "LLM rerank error rate > 5%"

      - alert: DH-WaypointDBPoolSaturated
        expr: dh_waypoint_postgres_pgxpool_acquired / dh_waypoint_postgres_pgxpool_total > 0.9
        for: 5m
        annotations:
          summary: "DH-Waypoint Postgres connection pool > 90% saturated"
```

Drop these in your alertmanager-rules file and reload Prometheus.

## Logging

Logs are stdout. There's no built-in log aggregator; use whatever your fleet uses:

- **Loki + Grafana** — pairs nicely with the dashboard import above
- **journald** — if running outside Docker
- **ELK / OpenSearch** — for larger fleets
- **Vector / Fluent Bit** — log shippers that route to any of the above

`LOG_LEVEL=info` is the production default. `debug` is loud (every Torznab query, every classifier decision) — useful for incident response, not for steady-state.

## Healthchecks

| Endpoint | Use |
|---|---|
| `/healthz` | Returns `200` if DB + queue + DHT are all OK. Boolean liveness check. |
| `/metrics` | The actual signal of "is the crawler making progress" — `rate(dh_waypoint_dht_ktable_hashes_added_total[5m])` is the real liveness. |

For a docker compose `healthcheck:` block, use `/healthz`. For Prometheus alerting, use the metrics.

## Multi-instance

For multiple DH-Waypoint instances (rare for self-hosters; common at operator scale), use distinct `instance` labels per scrape target:

```yaml
- job_name: dh-waypoint
  static_configs:
    - targets: ['dh-waypoint-eu:3333']
      labels: { instance: dh-waypoint-eu, service: dh-waypoint }
    - targets: ['dh-waypoint-us:3333']
      labels: { instance: dh-waypoint-us, service: dh-waypoint }
```

The Grafana dashboard supports per-instance filtering via a template variable.

## See also

- [Reference / Metrics](../reference/metrics.md) — full metric catalog
- [Operations / Security](security.md) — `/metrics` is unauthenticated; reverse-proxy if exposed
- [Operations / Upgrade](../../examples/README.md#upgrading)
