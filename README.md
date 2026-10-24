<h1 align="center">DH-Waypoint</h1>

<p align="center">
  <strong>A self-hosted BitTorrent DHT crawler and indexer for the *arr stack.</strong><br>
  Descended from bitmagnet, it learns from what your *arrs grab and serves its catalogue over Torznab, GraphQL and REST.
</p>

<p align="center">
  <a href="https://norvitech.com"><img alt="NorviTech Suite" src="https://img.shields.io/badge/NorviTech-Suite-FD8024.svg"></a>
  <a href="https://github.com/spencernorton/DH-Waypoint/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/spencernorton/DH-Waypoint/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/spencernorton/DH-Waypoint/releases"><img alt="Latest release" src="https://img.shields.io/github/v/tag/spencernorton/DH-Waypoint?label=release&sort=semver"></a>
  <a href="#install"><img alt="Docker Compose" src="https://img.shields.io/badge/install-docker%20compose-2496ed.svg?logo=docker&logoColor=white"></a>
  <a href="LICENSE"><img alt="MIT licence" src="https://img.shields.io/badge/licence-MIT-blue.svg"></a>
  <a href="https://buy.stripe.com/8x26oH2U44f65TRe574wM04"><img alt="Donate" src="https://img.shields.io/badge/donate-Stripe-635bff.svg?logo=stripe&logoColor=white"></a>
</p>

```mermaid
flowchart LR
    DHT[Public DHT and peers] --> Core[DH-Waypoint]
    Core --> DB[(PostgreSQL catalogue)]
    DB --> APIs[Torznab, GraphQL, REST]
    APIs --> Arr["Prowlarr and the *arrs"]
    Arr -- grabs and imports --> Core
    Core -. jobs .-> Proc[Optional processors]
    Proc -. proposals .-> Core
```

DH-Waypoint crawls the public BitTorrent DHT, keeps the torrent metadata it
finds in PostgreSQL and serves it to Prowlarr, Sonarr, Radarr, Lidarr and
Readarr as a Torznab indexer. Webhooks from those apps tell it what you
actually grabbed and imported, and that evidence outranks anything it guesses.
It is a headless Go service with no browser interface, forked in April 2026
from [bitmagnet](https://github.com/bitmagnet-io/bitmagnet).

## What it does

**It indexes v1 and v2 torrents.** The crawler samples the DHT (BEP 5 and 51)
and fetches metadata from peers (BEP 9 and 10); v1, pure v2 and hybrid torrents
keep their complete identities, and `.torrent` files can be imported directly.
Payloads are never downloaded, and opt-in BEP 15 tracker scrapes keep seeder
counts current.

**Your \*arr stack is the ground truth.** \*arr webhooks, an \*arr history
poller and a qBittorrent poller record what you grab and import in an
append-only evidence ledger, and a label backed by a real grab overrides the
classifier.
Opt-in success priors and liveness tracking then re-rank Torznab results and
drop dead swarms.

**Processing is as light as you want.** `PROCESSING_MODE=basic`, the default,
stamps a coarse category from file extensions; `full` adds the deterministic
classifier with anime-aware title parsing and TMDB matching; `raw` stores
metadata untouched. Heavier work, such as an LLM or the
[Armiger](https://github.com/spencernorton/armiger) classifier, runs as a
separate HTTP processor that you register by dropping a plugin file into
`./plugins`. Processors only propose: the core validates every proposal and
never lets one delete a record.

**Curation hides; it does not delete.** Content filtering and retention
report what they would remove through `would_*` metrics, while the opt-in
adult filter and name policy keep releases out of Torznab and GraphQL results
without deleting them. A CSAM blocklist, fed from
lists you choose, is checked before any metadata fetch.

**The APIs are standard and bounded.** Torznab for the \*arrs, GraphQL over
JSON `POST`, a REST read API with an OpenAPI document, and a raw REST v2
catalogue. Request bodies, selections and page sizes are capped before any
resolver runs, and catalogue work stops at a 30-second deadline.

**You can see what it is doing.** Every subsystem exports `dh_waypoint_*`
Prometheus metrics, `pgstats` reports table bloat, vacuum lag and pool
saturation, and Grafana dashboards ship in [`observability/`](observability/).

## Install

### Docker — Docker Compose

```sh
git clone https://github.com/spencernorton/DH-Waypoint.git
cd DH-Waypoint
cp examples/.env.example .env    # set POSTGRES_PASSWORD; nothing else is required
docker compose up -d
docker compose exec dh-waypoint dh-waypoint doctor
```

This runs the published `ghcr.io/spencernorton/dh-waypoint` image (pin a
release with `DH_WAYPOINT_VERSION`) and PostgreSQL 16, with HTTP on port 3333
and the peer protocol on 3334 TCP and UDP. Add `http://<host>:3333/torznab/`
as a Torznab indexer in Prowlarr or each \*arr ([per-app guides](docs/integrations/prowlarr.md)),
and point their webhooks at `http://<host>:3333/evidence/arr/<instance>` with
the `EVIDENCE_WEBHOOK_SECRET` from your `.env` ([evidence](docs/evidence.md)).
Set `TORZNAB_API_KEY`, and keep the other endpoints on a trusted network or
behind a proxy ([security](docs/operations/security.md)).

### From source

`examples/docker-compose.public.yml` builds the image from your checkout and
runs in `full` mode; [the quickstart](docs/quickstart.md) covers both stacks.

The published image is `linux/amd64` only. There is no browser interface,
account system or hosted site here, and the [LLM processor](plugins/llm/README.md)
is built from source; it has no published image. `main` is ahead of the latest
release: [`CHANGELOG.md`](CHANGELOG.md) lists what is unreleased, including
BitTorrent v2 indexing, the REST v2 catalogue and the external LLM processor.

## Documentation

- [Documentation index](docs/index.md): every guide in one place
- [Quickstart](docs/quickstart.md) and [configuration](docs/configuration.md)
- [API guide](docs/api-guide.md), with [Torznab](docs/reference/torznab-api.md), [GraphQL](docs/reference/graphql-api.md), [metrics](docs/reference/metrics.md) and [CLI](docs/reference/cli.md) references
- [Evidence](docs/evidence.md): wiring the \*arrs and qBittorrent into labels, ranking and liveness
- [Processors](docs/processors/plugin-files.md): plugin files, the [v1 protocol](docs/processors/v1.md), the [LLM processor](docs/processors/llm.md) and [Armiger](docs/armiger-integration.md)
- [BitTorrent v2](docs/bittorrent-v2.md): identities, the raw REST v2 catalogue and processor compatibility
- [Architecture](docs/architecture.md) and [operations](docs/OPERATIONS.md): internals, capacity, backups and upgrades
- [Benchmarks](docs/project/benchmarks.md) and [what changed from bitmagnet](docs/project/improvements.md)
- [Legal disclaimer](docs/legal/disclaimer.md) and [CSAM defence](docs/csam-defense.md)
- [Changelog](CHANGELOG.md) and [release process](docs/RELEASING.md)

## Where your data lives

| Path | Purpose |
|---|---|
| `dh-waypoint-postgres` volume | The database: catalogue, evidence ledger, queues and processor state |
| `dh-waypoint-config` volume, `/root/.config/bitmagnet` | Optional `config.yml` and `classifier.yml` |
| `dh-waypoint-data` volume, `/root/.local/share/bitmagnet` | Rotated log files, when file logging is enabled |
| `./plugins`, read-only | The plugin files that register processors |

The crawler joins the public DHT and connects to peers, which see this host's
IP address. Trackers, TMDB, blocklist feeds and exports, your \*arrs and
processors are contacted only when you configure them.

## Contributing and support

- Bugs and ideas: [open an issue](https://github.com/spencernorton/DH-Waypoint/issues/new/choose). Questions: [Discussions](https://github.com/spencernorton/DH-Waypoint/discussions).
- Security: [report a vulnerability privately](https://github.com/spencernorton/DH-Waypoint/security/advisories/new) ([SECURITY.md](SECURITY.md)). There is no e-mail address; that is deliberate.
- Code: read [CONTRIBUTING.md](CONTRIBUTING.md) first. Pull requests are reviewed and squash-merged here, and changes ship in tagged releases.
- If DH-Waypoint earns a place in your stack, you can [support its development](https://buy.stripe.com/8x26oH2U44f65TRe574wM04).

## Development

```sh
go vet ./...
go test -race ./...
python3 scripts/check_headless_boundary.py
python3 scripts/check_processor_boundary.py
task build    # ./DH-Waypoint, versioned from VERSION
task lint
```

PostgreSQL integration tests run when `DH_WAYPOINT_TEST_POSTGRES_DSN` points at
a disposable database ([development setup](docs/development/setup.md)).

## Licence

[MIT](LICENSE) © 2023 bitmagnet contributors, © 2026 DH-Waypoint contributors.

DH-Waypoint is a fork of [bitmagnet-io/bitmagnet](https://github.com/bitmagnet-io/bitmagnet)
at [`2b9e8ea`](https://github.com/bitmagnet-io/bitmagnet/commit/2b9e8eadd34c037830d1fa7470b5ef2746cd6388)
and keeps its licence and notices ([NOTICE](NOTICE)). The DHT crawler, metadata
fetcher, CEL classifier and the PostgreSQL and GraphQL foundations are
upstream's work.

---

<p align="center">
  <a href="https://norvitech.com"><img alt="Part of the NorviTech Suite — open-source apps for the Linux desktop and the self-hosted stack" src="https://norvitech.com/assets/banner.svg" width="640"></a>
</p>

<p align="center">
  <a href="https://github.com/spencernorton/helios">Helios</a> ·
  <a href="https://github.com/spencernorton/DH-Waypoint">DH-Waypoint</a> ·
  <a href="https://github.com/spencernorton/armiger">Armiger</a> ·
  <a href="https://github.com/spencernorton/xnote">XNote</a> ·
  <a href="https://github.com/spencernorton/xnote-placement">XNote Placement</a> ·
  <a href="https://github.com/spencernorton/snipsnap">SnipSnap</a> ·
  <a href="https://github.com/spencernorton/conductor">Conductor</a> ·
  <a href="https://github.com/spencernorton/norvi-os">NorviOS</a> ·
  <a href="https://github.com/spencernorton/indigo">Indigo</a> ·
  <a href="https://github.com/spencernorton/roadtrack">Road Track</a> ·
  <a href="https://norvitech.com">norvitech.com</a>
</p>
