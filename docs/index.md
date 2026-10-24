# DH-Waypoint backend documentation

DH-Waypoint is a self-hosted BitTorrent DHT crawler, content indexer and processing
backend. It builds on [bitmagnet](https://github.com/bitmagnet-io/bitmagnet) with
matching, evidence learning, filtering, scrubbing, swarm observations and cleanup.
The public distribution is MIT-licensed and runs independently of a browser site.

## Start here

- [Quickstart](quickstart.md): standalone backend and PostgreSQL
- [API guide](api-guide.md): working GraphQL, REST, Torznab and plugin calls in minutes
- [Plugin files](processors/plugin-files.md): drag-in filters, categories and services; [connecting an LLM](processors/llm.md); [integrating Armiger & Armiger Ultra](armiger-integration.md)
- [Private torrents and tracker](private-tracker.md): create, upload, personalised downloads, built-in tracker
- [Configuration](configuration.md): environment settings and safe opt-in
- [Operations](OPERATIONS.md): health, capacity, backups and upgrades
- [Migrating to 4.0](migration-v4.md): separate backend and site releases

## Processing and integrations

- [Core Architecture Guide](architecture.md) and [Backend Overview](concepts/architecture.md)
- [External Processor HTTP Webhook Contract](processor-contract.md) and [Armiger & Armiger Ultra Integration](armiger-integration.md)
- [Processor operations](processors/operations.md), [operator API](processors/operator-api.md),
  [migration](processors/migration.md) and [validation](processors/validation.md)
- [DHT crawler](concepts/dht-crawler.md)
- [BitTorrent v2](bittorrent-v2.md): complete identities, raw REST v2 and processor compatibility
- [Classification](concepts/classification.md)
- [Evidence](evidence.md) and [compatibility](concepts/compatibility.md)
- [Swarm health](concepts/swarm-health.md) and [wantbridge](concepts/wantbridge.md)
- Client integrations: [Prowlarr](integrations/prowlarr.md), [Sonarr](integrations/sonarr.md),
  [Radarr](integrations/radarr.md), [Lidarr](integrations/lidarr.md) and [Readarr](integrations/readarr.md)
- [Improvements](project/improvements.md) and [benchmarks](project/benchmarks.md)

## Policy, recovery and design

- [Consumer visibility](serving-exclusions.md), [release-name eligibility](RELEASE_NAME_POLICY.md)
  and [release-name metadata claims](release-metadata.md)
- [Catalogue removal foundation](catalogue-recovery.md) and
  [policy shadow review](catalogue-policy-shadow.md)
- [Catalogue search design](design/catalogue-search.md),
  [processor operations validation](processors/operations-validation.md) and the
  [glossary](concepts/glossary.md)

## Reference and development

- [GraphQL](reference/graphql-api.md), [Torznab](reference/torznab-api.md),
  [metrics](reference/metrics.md) and [CLI](reference/cli.md)
- [Development setup](development/setup.md) and [release process](RELEASING.md)
- [Security](operations/security.md) and [CSAM defense](csam-defense.md)
- [Monitoring](operations/monitoring.md), [FAQ](faq.md) and
  [troubleshooting](troubleshooting.md)

Attribution to upstream contributors is preserved in [NOTICE](../NOTICE).
