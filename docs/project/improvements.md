# Backend improvements over upstream

DH-Waypoint preserves the DHT and indexing foundation of
[bitmagnet](https://github.com/bitmagnet-io/bitmagnet). The public backend adds
processing and operational features that run with configuration you supply.

| Area | Backend behavior | Reference |
|---|---|---|
| Evidence | Configurable *arr webhooks/history and qBittorrent observations derive canonical labels | [Evidence](../evidence.md) |
| Classification | Processing modes, evidence preempt, CEL rules and optional external processors | [Classification](../concepts/classification.md) |
| Matching | Shared title normalization, anime aliases and metadata matching | [Benchmarks](benchmarks.md) |
| Curation | Deterministic content filters, verdicts, junk quarantine and safe cleanup gates | [Configuration](../configuration.md) |
| Retention | Dry-run purge reports for stale metadata; removal is held | [Configuration](../configuration.md) |
| Swarm observations | Tracker scrapes keep authoritative zero distinct from unknown | [Swarm health](../concepts/swarm-health.md) |
| Defense | Pre-fetch double-hash blocklist and post-fetch checks | [CSAM defense](../csam-defense.md) |
| Observability | Prometheus families, database observations and generic Grafana resources | [Monitoring](../operations/monitoring.md) |
| Distribution | Go-only runtime, checked version/source identity and independent site lifecycle | [Releasing](../RELEASING.md) |

Optional modes are not evidence of live processing. Use the documented metrics,
evaluation methodology and shadow/dry-run stages to qualify behavior in your
own deployment. Browser/account functionality belongs to a separate repository
and is not an improvement shipped in this backend artifact.
