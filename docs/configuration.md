# Configuration

DH-Waypoint is configured through environment variables and, optionally,
a YAML file — no config file is required for any deployment shape.
The runtime resolves settings at startup (precedence:
environment > `EXTRA_CONFIG_FILES` > `./config.yml` > XDG
`bitmagnet/config.yml` > defaults), and keeps everything in a single
resolved-config tree that you can introspect with
`dh-waypoint config show`. `dh-waypoint init` scaffolds a starter
`config.yml` and `.env`; YAML keys are the dotted paths `config show`
prints, nested (for example `processing: {mode: basic}`). Unknown keys in YAML files fail startup — a typo is never silently ignored.

This page covers the common settings. `dh-waypoint config show` lists every key the binary reads; its environment variable is the path upper-cased with `.` replaced by `_` (`dht_crawler.scaling_factor` → `DHT_CRAWLER_SCALING_FACTOR`). Variables marked *Compose only* are read by the bundled Compose files, not by the binary.

For first-time setup see [Quickstart](quickstart.md). For production layouts see [examples/README.md](../examples/README.md). For the security implications of each auth knob see [operations/security.md](operations/security.md).

## Secrets: prefer the `_FILE` form

**Every variable the binary reads also accepts a `<NAME>_FILE` form** that names a file to read the value from, the same convention the official `postgres` and `mysql` images use:

```yaml
environment:
  PROCESSORS_ADMIN_TOKEN_FILE: /run/secrets/processors_admin_token
volumes:
  - /srv/secrets/dh-waypoint/processors_admin_token:/run/secrets/processors_admin_token:ro
```

Use it for every credential. A value passed as a plain environment variable is stored in container metadata and printed in cleartext by `docker inspect`, so anyone who can reach the docker socket — and anything that scrapes container metadata, including stack-backup tooling — can read it. A mounted file is not in that metadata.

Rules:

- The plain variable wins when it is set to a non-empty value. An **empty** plain value is treated as absent, so a leftover `FOO=` (which is what compose's `${FOO:-}` expands to) cannot silently shadow the file and disable the feature it configures.
- A `_FILE` path that is missing, unreadable, or empty is a **startup error**, not a fall-back to the default. Failing loudly is deliberate: a silently-empty API key reads as "tier disabled" and has caused real outages here.
- Contents are whitespace-trimmed, so `echo "$KEY" > secret` works.
- On a coercion failure the value is not echoed into the error, unlike the plain-env path.

## Required

The single hard requirement is the database password. The bundled Compose files refuse to start without it.

| Variable | Default | Purpose |
|---|---|---|
| `POSTGRES_PASSWORD` | *(required)* | Postgres credential. Pick something long and random — this is the only thing standing between an attacker on the host and your indexed corpus. |

## Networking

Two ports and a Postgres connection. The bundled Compose files set working values; override only if you have a port conflict or external Postgres.

| Variable | Default | Purpose |
|---|---|---|
| `DH_WAYPOINT_HTTP_PORT` | `3333` | *Compose only:* host port for the HTTP API, which the binary serves on `HTTP_SERVER_LOCAL_ADDRESS` (default `:3333`). Hosts `/graphql`, `/torznab/api`, `/metrics`, `/import`, `/evidence/arr/*`. |
| `DH_WAYPOINT_PEER_PORT` | `3334` | *Compose only:* host port (TCP and UDP) for the BitTorrent peer protocol, which the binary serves on `DHT_SERVER_PORT` (default `3334`). Outbound-dominant; opening it inbound increases throughput but is not required. |
| `POSTGRES_HOST` | `localhost` | Hostname or IP. The bundled Compose files set `postgres`, the service name. |
| `POSTGRES_PORT` | `5432` | Postgres port. |
| `POSTGRES_NAME` | `bitmagnet` | Database name. The legacy schema name is preserved for compatibility with deployments that pre-date the rebrand; do not rename without a planned migration. |
| `POSTGRES_USER` | `postgres` | Postgres user. The bundled Compose files set `bitmagnet`, for the same compatibility reason as `POSTGRES_NAME`. |

## Authentication

By default DH-Waypoint's HTTP server trusts whoever connects. These variables gate the surfaces that face external clients.

| Variable | Default | Purpose |
|---|---|---|
| `TORZNAB_API_KEY` | *(none)* | When set, every `/torznab/api` request must include `apikey=<value>` (or an `X-Api-Key` header). With this and `TORZNAB_API_KEYS` empty, `/torznab` is open — fine on a tailnet or behind a reverse proxy with its own auth, **not fine on the open internet.** Constant-time compare. |
| `TORZNAB_API_KEYS` | *(none)* | Comma-separated `name:value` keys accepted alongside `TORZNAB_API_KEY`; the name labels `dh_waypoint_torznab_requests_total`. Malformed entries are skipped. |
| `EVIDENCE_WEBHOOK_SECRET` | *(none)* | Shared secret for the `*arr` webhook ingester. Mirror this value as the `X-Evidence-Token` Custom Header on every Sonarr/Radarr/Lidarr/Readarr Connect → Webhook configuration. Empty leaves `/evidence/arr/*` unauthenticated. |
| `HTTP_SERVER_API_KEY` | unset | Optional key gating GraphQL, `/import`, `/metrics` and `/api/v1/*` (`Authorization: Bearer` or `X-API-Key`). Torznab, tracker, processors, private and evidence APIs keep their own credentials; `/status` stays public. Unset preserves the open trusted-network posture. |

## Release-name eligibility

This policy is disabled by default. Enabling it rejects releases whose names
contain Han or Cyrillic script, clear combinations of adult tokens, or an
explicitly configured info hash. It uses the release name and existing adult
classification; it does not infer audio language or origin from metadata,
accents, transliteration or support-file paths. Standalone ambiguous terms and
the film title *xXx* remain eligible for the existing checks.

| Variable | Default | Purpose |
|---|---|---|
| `NAME_POLICY_ENABLED` | `false` | Apply the shared deterministic eligibility policy to consumer views and processing. Stored data and recovery evidence remain intact. |
| `NAME_POLICY_EXCLUDED_INFO_HASHES` | *(empty)* | Bounded comma-separated list of exact info hashes excluded by operator policy. Invalid hashes fail startup. |
| `NAME_POLICY_INTERNAL_CHECK_TOKEN` | *(empty)* | A 32–256 byte bearer credential for internal eligibility checks. Prefer the `_FILE` form. The credential is redacted in config display and excluded from policy fingerprints. |

Both internal POST endpoints require an enabled policy and configured bearer
credential; otherwise they return 404. `/internal/name-policy/check` accepts at
most 32 trusted-caller release names for a separate private catalogue.
`/internal/name-policy/check-public` accepts only `infoHashes`, resolves their
actual stored names through the mandatory consumer view, and cannot accept a
substitute caller name. It bounds requests to 32 hashes/4 KiB and lookup to five
seconds. Unknown, private or suppressed records are ineligible for public grabs.
Neither response echoes names or hashes. Successful responses contain `enabled`,
`version` and results bound to request positions with `eligible` and `reason`.
These checks do not grant private membership or download authority.

Metadata repairs also recheck current eligibility, including previously frozen
plans. A plan for an excluded release remains held. Existing privacy,
manual/reference/wanted/bitgrab, identity and non-NULL field protections remain
in force.

## Processing mode

One switch decides how much deterministic processing the core runs
before optional processor plugins see a torrent. This is the
master "turn the built-in brain off" control for installs whose
classification and filtering live in plugins or a custom dataset.

| Variable | Default | Purpose |
|---|---|---|
| `PROCESSING_MODE` | `basic` (`full` through 5.7.1) | `full`: complete CEL classifier workflow (title parsing, matching, TMDB/anime enrichment) plus the content filter when separately enabled. `basic`: no workflow, no metadata providers, no network — core stamps only a coarse content type from file-extension byte majority (episode pattern separates TV from movies); hints still win; nothing is ever deleted by classification. `raw`: metainfo persisted untouched, content type stays unknown until a plugin proposes one; classifier and content filter never run. Invalid values fail startup. Name policy and the CSAM pre-fetch blocklist are independent safety controls and are unaffected. Changing the mode stales outstanding processor pipelines by design. |

## Classifier

The classifier runs locally with no external dependencies. TMDB enrichment is optional and improves movie/TV title resolution. It runs only in `PROCESSING_MODE=full`.

| Variable | Default | Purpose |
|---|---|---|
| `TMDB_API_KEY` | *(none)* | Free TMDB v3 API key. Enriches movie + TV records with title, release year, and episode metadata. Without a key, also set `TMDB_ENABLED=false` so the classifier skips TMDB lookups instead of failing them. Get a key at <https://www.themoviedb.org/settings/api>. |

## Optional processors

No model/provider configuration is registered by the core. Optional services
own inference runtimes, prompts, credentials, caches and budgets. See
[processor configuration and lifecycle](processors/v1.md) and the
[authenticated operator API](processors/operator-api.md).

| Variable | Default | Purpose |
|---|---|---|
| `PROCESSORS_ADMIN_TOKEN` | unset | Bearer for registry/status/jobs API; unset returns 503 only on that API. |
| `PROCESSORS_PLUGINS_DIR` | `plugins` | Directory of `*.plugin.yml` registration files reconciled every 30s (see [plugin files](processors/plugin-files.md)). Empty disables directory sync; a missing directory is a no-op. |
| `PROCESSORS_CREDENTIAL_DIRECTORY` | `/run/secrets/dh-waypoint-processors` | Dedicated read-only transport secret directory; files read per request. |
| `PROCESSORS_CONCURRENCY` | `2` | Global active optional requests across replicas sharing this database. |
| `PROCESSORS_QUEUE_CAPACITY` | `1000` | Pending optional pipeline jobs. |
| `PROCESSORS_RETAINED_JOBS` | `10000` | Maximum retained optional jobs; admission pauses at the bound. |
| `PROCESSORS_REQUEST_TIMEOUT` | `10s` | Hard request deadline; allowed 100ms–60s. |
| `PROCESSORS_MAX_ATTEMPTS` | `3` | Per-step dispatch limit, at most 5. |
| `PROCESSORS_POLL_INTERVAL` | `1s` | Optional admission/worker cadence. |
| `PROCESSORS_SCAN_BATCH` | `32` | Maximum existing torrents inspected per catch-up scan, at most 256. |
| `PROCESSORS_RETENTION` | `168h` | Terminal job retention, at most 30 days. |

The `processors` worker is included by `worker run --all`. Explicit worker lists
must include it once when migrating the core. Registrations are stored in
PostgreSQL and discovered without a subsequent restart. The core and optional
queues have independent worker slots. Transport authentication references
`DH_WAYPOINT_PROCESSOR_TOKEN_*`; no credential value is returned by the registry.

## Private torrents and tracker

Optional and off by default; see [private tracker guide](private-tracker.md).

| Variable | Default | Purpose |
|---|---|---|
| `TRACKER_ENABLED` | `false` | Answer `/announce/<passkey>` and `/scrape/<passkey>`. Requires at least one passkey. |
| `TRACKER_PASSKEYS` | *(none)* | Comma-separated tracker passkeys (8–256 chars each), embedded in personalised announce URLs. Prefer the `_FILE` form. |
| `TRACKER_ADMIN_TOKEN` | unset | Bearer token for `/api/private/v1` (upload/detail/admin download); unset returns 503 on that API only. The file API works without the tracker. |
| `TRACKER_PUBLIC_BASE_URL` | *(from request)* | Externally reachable base used to build personalised announce URLs on download. |
| `TRACKER_ANNOUNCE_INTERVAL` | `30m` | Client announce interval (1m–6h); peers expire after twice this. |
| `TRACKER_MAX_PEERS` | `50` | Peers returned per announce (1–200). |

## Crawler

A single multiplicative knob covers DHT worker concurrency. Higher values mean more peers, more memory, more CPU.

| Variable | Default | Purpose |
|---|---|---|
| `DHT_CRAWLER_SCALING_FACTOR` | `10` | Multiplier applied to internal DHT worker counts. `1` is safe on a 4 GB host. `4`–`10` is appropriate on a beefier box; anything higher should be informed by `dh_waypoint_dht_*` Prometheus signal. |
| `DHT_SCALING_FACTOR` | `1` | *Compose only:* the bundled Compose files pass it to `DHT_CRAWLER_SCALING_FACTOR`; the binary ignores it. Note there is **no** `BITMAGNET_` prefix on any key: the resolver builds keys from the config section plus the snake_cased Go field name, and an unmatched environment variable is ignored in silence rather than rejected. |

## Logging

| Variable | Default | Purpose |
|---|---|---|
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. `info` is fine for steady-state; `debug` is loud and only useful while reproducing a specific issue. |

## CSAM defense

A pre-fetch double-hashed blocklist filters infohashes before DH-Waypoint ever does a BEP-9 metadata fetch — closing the swarm-touching exposure window for the one category of content where post-fetch classification is too late. Defaults are safe: enabled with no feeds is a NoOp until you opt in to a feed you trust. Full architecture is in [csam-defense.md](csam-defense.md).

| Variable | Default | Purpose |
|---|---|---|
| `CSAM_BLOCKLIST_ENABLED` | `true` | Master toggle. Leaving it on with no feeds set has zero runtime cost. |
| `CSAM_BLOCKLIST_FEED_URLS` | *(none)* | Comma-separated list of community blocklist feed URLs. Each feed is fetched, parsed (one double-hash per line), and merged into a bloom filter. Empty = NoOp. |
| `CSAM_BLOCKLIST_EXPORT_ENABLED` | `true` | When the post-fetch classifier flags a CSAM-banned title, append the double-hashed infohash to a local JSONL log. On by default; the file never leaves the host unless `EXPORT_UPSTREAM_URL` is also set. |
| `CSAM_BLOCKLIST_EXPORT_UPSTREAM_URL` | *(none)* | Optional outbound endpoint for community contribution. POSTs each new double-hash observation. Off by default — opt in only if you have an endpoint you trust. |

## Content filter

The existing deterministic filter remains opt-in with `CONTENT_FILTER_ENABLED`
and `CONTENT_FILTER_ENFORCE`. Its language tags, script checks, extension/type
policy, lossy-audio rule and anime exceptions remain in core. Shadow mode records
would-drop decisions. Destructive dispositions still require qualified catalogue
recovery; this refactor does not lift that hold. Model-driven language review and
junk judgement now belong to optional services. Existing English-audio claims
retain their historical attribution and are not independently verified audio.

See `internal/classifier/contentfilter/config.go` for deterministic options and
[the migration guide](processors/migration.md) for retired settings.

## Retention

Periodic deletion of torrents that haven't been seen in DHT announcements for a long time. Same two-stage opt-in pattern as the content filter.

| Variable | Default | Purpose |
|---|---|---|
| `RETENTION_ENABLED` | `false` | Stage 1: dry run. The `would_purge` counter increments for each torrent that *would* be deleted. Nothing actually deletes. |
| `RETENTION_ENABLE_PURGE` | `false` | Stage 2: delete. Requires `RETENTION_ENABLED=true`. Held in current builds: each cycle logs a recovery-disabled error and deletes nothing. |

The default predicate is conservative: no canonical label, no evidence record, age > 60 days, `updated_at` older than 180 days, and every torrent source reports `seeders=0` observed within the last 30 days.

## Quarantine maintenance

`JUNKPURGE_MAINTENANCE_ENABLED=false` is the default. Enable it to run the
`quarantine` worker's bounded historical-snapshot expiry maintenance.
`JUNKPURGE_QUARANTINE_DAYS=30` controls the window. Expiry retains evidence;
plugins never receive a delete operation or unrestricted database access.

## Configurable evidence sources

The backend can poll generic *arr and qBittorrent endpoints you configure.
No source is attached by default. Empty base URLs leave the corresponding
source idle. The example stack omits these optional settings; add them to
your own environment or YAML configuration when needed.

| Variable | Default | Purpose |
|---|---|---|
| `EVIDENCE_SONARR_BASE_URL` / `EVIDENCE_SONARR_API_KEY` | empty | Sonarr history source |
| `EVIDENCE_RADARR_BASE_URL` / `EVIDENCE_RADARR_API_KEY` | empty | Radarr history source |
| `EVIDENCE_READARR_BASE_URL` / `EVIDENCE_READARR_API_KEY` | empty | Readarr history source |
| `EVIDENCE_LIDARR_BASE_URL` / `EVIDENCE_LIDARR_API_KEY` | empty | Lidarr history source |
| `EVIDENCE_QB_ALPHA_BASE_URL` / `EVIDENCE_QB_ALPHA_USERNAME` / `EVIDENCE_QB_ALPHA_PASSWORD` | empty | First qBittorrent source |
| `EVIDENCE_QB_BETA_BASE_URL` / `EVIDENCE_QB_BETA_USERNAME` / `EVIDENCE_QB_BETA_PASSWORD` | empty | Second qBittorrent source |
| `EVIDENCE_ARR_POLL_INTERVAL` | `15m` | History polling cadence |
| `EVIDENCE_QB_POLL_INTERVAL` | `15m` | qBittorrent polling cadence |

See [evidence](evidence.md) for webhook authentication, evidence precedence,
liveness and outcome-prior settings. Source credentials belong in an ignored
environment file or secret manager. Network tunnels and human account services
are deployment choices rather than backend configuration.

## Live config inspection

Run `dh-waypoint config show` against your running container to dump every config path, type, current value, default, and the source resolver that produced it (env var, default, file, etc.). This is the canonical way to verify a configuration override took effect:

```bash
docker exec dh-waypoint dh-waypoint config show
```

Output is a wide table; pipe to `less -S` if your terminal is narrow. The `From` column tells you exactly where a value came from — invaluable when an override is silently being shadowed by a higher-precedence source.

See [reference/cli.md](reference/cli.md) for the full CLI surface.

## See also

- [Quickstart](quickstart.md)
- [Examples / self-hoster quickstart](../examples/README.md)
- [reference/cli.md](reference/cli.md)
- [reference/metrics.md](reference/metrics.md)
- [csam-defense.md](csam-defense.md)
- [operations/security.md](operations/security.md)

`release-fields-repair` operates on a bounded frozen cohort without calling a
workflow, metadata provider or model. Freeze an explicit JSON array of at most
1,000 info hashes with `freeze --cohort cohort.json --output plan.json`, inspect
the before/after fields, then use `apply --plan plan.json` for a dry run.
`--write` commits each row with its before/after journal. The frozen build,
parser, full source and name digests, target revision, current fields and absence of manual hints
must match. New advertised attribute JSON is filled only when the existing
value is NULL. Changing a non-null historical video source requires explicit
reviewed ownership evidence in that exact plan; it is never assumed to belong
to the parser.

A restart resumes using the journal. `rollback --plan plan.json` reports the
restoration; adding `--write` restores only unchanged journalled fields. Source
edits, later revisions or changed application facts refuse restoration. Field
repairs rebuild the dependent search projection from preserved name, file,
episode and catalogue identity facts using the existing projection helper. They
update corresponding committed application snapshots atomically, so
ordinary processing preserves the corrected facts after response retention.
The journal is retained even after rollback and blocks a destructive schema
downgrade. Retain the matching database backup and compatible binary.

Committed application facts are retained separately from expiring provider
bodies. Ordinary processing may preserve those facts after response retention
ends, including after a restart, only while current source, policy, target and
tags still match. A missing or changed fact holds processing for explicit
reconciliation. Preservation does not authorize another provider request and
is checked again inside the ordinary persistence transaction.
