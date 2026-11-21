# DH-Waypoint Plugin and Sidecar Adapter SDK

DH-Waypoint keeps its core crawler, database, and serving engine strictly headless and inference-free. Classification models, policy reviewers, stream bridges, and external feed importers run out-of-process over documented HTTP contracts.

## 1. Processor Plugins (`processorapi/v1`)

A processor plugin is any HTTP service implementing three endpoints:

- `GET /manifest`: Declares stable `processor` identity (`id`, `version`, `model_version`, `policy_version`, `artifact_digest`), supported `capabilities` (`classify`, `extract`, `match`, `policy`), and protocol contract `dh-waypoint.processor.v1`.
- `POST /process`: Receives a signed job `binding`, torrent metadata/files/swarm snapshot, and deadline; returns `outcome` (`completed`, `abstained`, `deferred`, `failed`), optional `proposal`, `reasons`, and `evidence`.
- `GET /health`: Returns HTTP `200` when ready to serve jobs.

See [docs/processor-contract.md](processor-contract.md) and [docs/processors/v1.md](processors/v1.md) for full schemas and invariants.

### Verifying a Plugin Before Registration

Run the built-in contract verifier against any local or remote plugin endpoint:

```sh
dh-waypoint plugin verify http://127.0.0.1:8091
# With bearer authentication:
dh-waypoint plugin verify --token "$PLUGIN_TOKEN" http://127.0.0.1:8091
```

`plugin verify` checks `/health`, validates `/manifest` schema and artifact-digest format, sends a synthetic job to `/process`, verifies echoed binding fields, and exits non-zero on any contract mismatch.

### File-Managed Registration (`plugins/*.plugin.yml`)

Drop a `*.plugin.yml` manifest into `PROCESSORS_PLUGINS_DIR` (`/plugins` in Docker Compose). DH-Waypoint syncs enabled manifests every 30 seconds and automatically re-registers a plugin when its upstream `artifact_digest` changes.

```yaml
enabled: true
id: my-custom-classifier
endpoint: http://127.0.0.1:8095
mode: apply # observe | apply
capabilities:
  - classify
  - extract
auth_token_env: MY_PLUGIN_TOKEN
```

### Security and Isolation Rules

- Bind plugin services to loopback (`network_mode: service:dh-waypoint` in Compose) so unauthenticated hosts cannot reach `/process`.
- Always protect cross-host plugin endpoints with per-plugin bearer tokens (`auth_token_env`).
- Never embed model weights, external API keys, or private deployment URLs into the core backend binary.
- For fail-closed serving gates, pin `SERVING_REQUIRED_PROCESSOR` + `SERVING_REQUIRED_PROCESSOR_ARTIFACT_DIGEST` (and optionally `SERVING_REQUIRED_PAYLOAD_VERIFIER_ARTIFACT_DIGEST` for piece-bound payload verification).

## 2. Standalone Sidecar Adapters

Consumer bridges and ingestion syncers talk solely to DH-Waypoint's public HTTP APIs and import zero internal Go packages:

- **Stremio Addon Bridge (`adapters/stremio`)**: Pure Go standard-library Stremio Addon Protocol v1 server querying `/torznab/api`. Enable in Docker Compose with `COMPOSE_PROFILES=stremio`.
- **Prowlarr / Torznab Sync (`adapters/prowlarr-sync`)**: Polls external Torznab RSS feeds and pushes batches to `POST /import?on_error=skip` (`X-Import-On-Error: skip`) so one malformed item never aborts a chunk. Enable with `COMPOSE_PROFILES=prowlarr-sync`.
- **REST API (`/api/v1/search`, `/api/v2/search`)**: Supports full-text search, IMDb/TMDb ID lookups, season/episode filtering, `resolution`, `video_source`, `video_codec`, `hdr`, `min_seeders`, and `order_by` sorting (`docs/reference/openapi.yaml` and `docs/reference/openapi-v2.yaml`).

## 3. Built-In Storage & Curation Plugin: LEAN MODE

Per-torrent `processorapi/v1` plugins receive one torrent at a time (`POST /process`) and cannot run cross-torrent competition, demote incumbents, manage standby failovers, or compact losing rows. For resource-constrained installations that want to keep only the single best release per movie or TV episode, DH-Waypoint ships the opt-in **LEAN MODE** plugin (`plugins/lean-mode/*.yml`):

- **Disabled by default (`LEAN_MODE_ENABLED=false`):** Zero runtime, SQL, or storage overhead when inactive.
- **Progressive phases:** `observe` (shadow materialization) -> `serve` (non-destructive projection per Torznab/REST profile or globally via `LEAN_MODE_SERVE_GLOBALLY=true`) -> double-locked `prune` (bounded compaction with `keep_runner_up` failover and config-digest `lean_tombstones` hooked into `dhtcrawler` and `importer`).
- **Declarative 3-tier YAML schema:** `required` hard gates, `preferred` ordered weighted/lexicographic ranking with equivalence bands, and `if_found` tie-breakers anchored by deterministic `info_hash` ordering.

See [docs/lean-mode.md](lean-mode.md) for the full configuration schema, CLI workflow, presets, and disk reclamation guide.
