# Headless backend quickstart

This stack builds the backend from source. For the fastest install,
the repository-root `docker-compose.yml` pulls the published image
instead; see the [quickstart](../docs/quickstart.md).

The public stack runs the DH-Waypoint Go backend and PostgreSQL. It includes DHT
crawling, classification, curation, Torznab, GraphQL and metrics. The hosted
site, accounts, library and admin pages run independently.

## Start and verify

```bash
git clone https://github.com/spencernorton/DH-Waypoint.git
cd DH-Waypoint
cp examples/.env.example examples/.env.public
# Set a unique POSTGRES_PASSWORD in examples/.env.public.
docker compose -f examples/docker-compose.public.yml \
  --env-file examples/.env.public up -d --build
curl --fail http://localhost:3333/metrics
curl --fail http://localhost:3333/graphql \
  -H 'Content-Type: application/json' \
  --data '{"query":"{ version }"}'
```

There is no browser UI or GraphQL playground in this image. The backend
runs without the hosted site, any account system, or private hardware.
Optional metadata providers and processors use your own configuration and credentials.

## Endpoints

| Path | Purpose | Authentication |
|---|---|---|
| `POST /graphql` | Query/mutation API | Trusted proxy required when exposed |
| `/torznab` | Torznab feed for *arr clients | `TORZNAB_API_KEY` when set |
| `/metrics` | Prometheus metrics | Trusted proxy required when exposed |
| `POST /import` | NDJSON bulk import | Trusted proxy required when exposed |
| `/evidence/arr/*` | *arr webhook ingestion | `X-Evidence-Token` when `EVIDENCE_WEBHOOK_SECRET` is set |

PostgreSQL has no published host port. Protect the HTTP API independently
before exposing it outside your trusted network. Set `TORZNAB_API_KEY` before
sharing the feed. Content filtering and retention
remain opt-in and report through shadow metrics; catalogue deletion is
disabled in current releases ([catalogue removal](../docs/OPERATIONS.md#catalogue-removal)).

The persistent volumes store PostgreSQL, core configuration and core data.
Existing upstream-compatible paths under `/root/.config/bitmagnet` and
`/root/.local/share/bitmagnet` are preserved.

## Upgrade and recovery

Build a checked immutable release and retain its image digest. Back up
PostgreSQL and core state before upgrading, then verify metrics, migrations
and Torznab compatibility. Rebuilding preserves the named volumes:

```bash
docker compose -f examples/docker-compose.public.yml \
  --env-file examples/.env.public up -d --build
```

`docker compose -f examples/docker-compose.public.yml down` stops the stack
while retaining state. Adding `--volumes` deletes all three named volumes,
including the indexed corpus.

See [operations](../docs/OPERATIONS.md), [configuration](../docs/configuration.md)
and [security](../SECURITY.md) for detailed guidance.
