# Quickstart: standalone backend

Deploy the Go backend and PostgreSQL, verify its machine APIs, then connect an
indexer client. The public stack does not serve a browser page.

## Prerequisites

Use Docker and Docker Compose, persistent disk, and outbound TCP/UDP access
for DHT and metadata retrieval. PostgreSQL 14+ is required; the example includes
PostgreSQL 16. Start with modest crawl settings and measure memory/disk growth
before sizing a large corpus. Bootstrap time depends on the network.

## Source and configuration

Fastest path — published image, no source build:

```sh
git clone https://github.com/spencernorton/DH-Waypoint.git
cd DH-Waypoint
cp examples/.env.example .env
openssl rand -hex 32   # paste as POSTGRES_PASSWORD in .env
openssl rand -hex 32   # paste as TORZNAB_API_KEY in .env
docker compose up -d
docker compose exec dh-waypoint dh-waypoint doctor
```

The root `docker-compose.yml` pulls `ghcr.io/spencernorton/dh-waypoint`,
runs in `basic` processing mode by default (coarse categories in
core; classification/filtering belong to plugins — set
`PROCESSING_MODE=full` for the built-in classifier), and mounts
`./plugins` for drag-in `*.plugin.yml` plugin files. With a Go
toolchain available, `dh-waypoint init` generates the `.env` secrets
and a starter `config.yml` for you.

To build from source instead, use the examples stack (it defaults
to `PROCESSING_MODE=full`):

```sh
cp examples/.env.example examples/.env.public   # set POSTGRES_PASSWORD
docker compose -f examples/docker-compose.public.yml --env-file examples/.env.public up -d --build
```

Compose requires a nonempty database password. A nonempty Torznab key protects indexer requests;
it does not protect GraphQL, import or metrics.

Expect `dh-waypoint` and `postgres` to become healthy. Inspect service logs if either
restarts or fails its probe. Peer traffic uses the configured port, default 3334 TCP/UDP.

## Verify the APIs

```sh
curl --fail http://localhost:3333/metrics
curl --fail -H 'Content-Type: application/json'   --data '{"query":"{ __typename }"}' http://localhost:3333/graphql
# Supply the same private key configured in your environment file.
curl --fail "http://localhost:3333/torznab/api?t=caps&apikey=$TORZNAB_API_KEY"
```

GraphQL should return a JSON envelope, and Torznab a capabilities document.
Watch `dh_waypoint_dht_crawler_persisted_total` and container logs for crawl progress.
An early empty search may reflect warmup; do not assume a fixed deadline.

## Connect a client

In Sonarr/Radarr/Prowlarr, add a custom Torznab indexer with the backend's
reachable `http://<host>:3333/torznab` URL and the configured API key. A client
in another container cannot use your host's `localhost`. Expand the loopback
binding only with an appropriate trusted-network or reverse-proxy policy.

Test capabilities from the client's network before saving the indexer. Use
[per-app guides](integrations/sonarr.md) for categories and search behavior.
Configure [evidence webhooks](evidence.md) with their separate secret to feed
successful acquisitions back into matching and ranking.

## Continue

- [Configuration](configuration.md)
- [API reference](reference/graphql-api.md)
- [Classification](concepts/classification.md)
- [Security](operations/security.md) and [monitoring](operations/monitoring.md)
- [Troubleshooting](troubleshooting.md)
- [Migrating from 3.x](migration-v4.md)
