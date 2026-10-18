# Talking to DH-Waypoint: API guide

DH-Waypoint core has no browser UI. Everything is a machine API on one
HTTP port (default `3333`), plus the `dh-waypoint` CLI. This guide is
the fastest route from a running container to working calls; the
exhaustive references are linked at the end.

## The surfaces

| Surface | Path | Auth | Use it for |
|---|---|---|---|
| GraphQL | `POST /graphql` | `HTTP_SERVER_API_KEY` when set (Bearer or `X-API-Key`) | Search, facets, files, health — the fullest catalogue API for custom UIs/clients |
| REST | `GET /api/v1/*` | same API key when set | Search, detail, files, stats for clients that do not want GraphQL; spec at `/api/v1/openapi.yaml` |
| Torznab | `GET /torznab/api` | `apikey=` or `Authorization: Bearer` when `TORZNAB_API_KEY` is set | Sonarr/Radarr/Lidarr/Readarr/Prowlarr indexer feed |
| Processors admin | `/api/processors/v1/*` | `Authorization: Bearer $PROCESSORS_ADMIN_TOKEN` (503 when unset) | Register/pause/remove plugins, job status |
| Evidence webhook | `POST /evidence/arr/<instance>` | `X-Evidence-Token: $EVIDENCE_WEBHOOK_SECRET` | Feed *arr grab/import outcomes back into matching |
| Import | `POST /import` | none built-in | Bulk NDJSON import of torrent records |
| Private files + tracker | `/api/private/v1/*`, `/announce/<passkey>`, `/scrape/<passkey>` | admin bearer token / member passkey | Private `.torrent` upload, personalised download, swarm tracking ([guide](private-tracker.md)) |
| Metrics / status | `GET /metrics`, `GET /status` | none built-in | Prometheus scrape, liveness |

All examples use `B=http://localhost:3333`.

## 1. Is it alive?

```sh
curl -s $B/status
curl -s $B/metrics | grep dh_waypoint_dht_crawler_persisted_total
curl -s $B/graphql -H 'Content-Type: application/json' \
  -d '{"query":"{ version health { status } }"}'
```

## 2. Search the catalogue (GraphQL)

```sh
curl -s $B/graphql -H 'Content-Type: application/json' -d '{
  "query": "query($q:String!){ torrentContent { search(input:{queryString:$q, limit:10}) { totalCount hasNextPage items { infoHash title contentType publishedAt torrentSource } } } }",
  "variables": {"q": "the matrix"}
}'
```

Notes that save debugging time:

- `items` are torrent-*content* rows (one classification per
  torrent). Files live under `torrent { files(input:{infoHashes:[...]}) }`.
- Private torrents never appear in GraphQL or Torznab results; the
  serving policy excludes them unconditionally.
- Facet counts come from `aggregations` on the same search result;
  `limit: 0` returns counts without rows.
- Introspection is enabled: point any GraphQL client (Insomnia,
  Altair, `gql` codegen) at `$B/graphql` and generate types from
  the live schema instead of hand-writing them.
- Full schema notes and more queries: [GraphQL reference](reference/graphql-api.md).

## 2b. The same search over REST

```sh
curl -s "$B/api/v1/search?q=the+matrix&type=movie&limit=10"
curl -s "$B/api/v1/torrents/<info_hash>"
curl -s "$B/api/v1/torrents/<info_hash>/files"
curl -s "$B/api/v1/stats"
curl -s "$B/api/v1/openapi.yaml"   # generate a client from this
```

Responses are stable snake_case JSON (`total_count`, `has_next_page`,
`items[]` with `info_hash`, `name`, `title`, `content_type`, `size`,
`seeders`, `published_at`, `magnet`, …). The REST surface is
read-only and uses the identical serving policy as GraphQL, so
private torrents never appear. Prefer GraphQL when you need facets,
grouping or mutations; prefer REST when you need a typed client in
minutes.

## 3. Feed an *arr (Torznab)

```sh
# capabilities (what Prowlarr checks first)
curl -s "$B/torznab/api?t=caps&apikey=$TORZNAB_API_KEY"

# free-text search, movies only (cat 2000)
curl -s "$B/torznab/api?t=search&q=the+matrix&cat=2000&limit=10&apikey=$TORZNAB_API_KEY"

# movie by TMDB id (identifier params beat free text)
curl -s "$B/torznab/api?t=movie&tmdbid=603&apikey=$TORZNAB_API_KEY"
```

Then add `$B/torznab` as a custom Torznab indexer in Prowlarr (or
each *arr) with the same key, and set the *arr webhook URL to
`$B/evidence/arr/<instance>` so grabs feed back into matching —
details in [evidence](evidence.md) and the
[Torznab reference](reference/torznab-api.md).

## 4. Manage plugins

Two equivalent routes. Files are the easy one: drop a
`*.plugin.yml` into the plugins directory and it is registered
within 30 seconds ([plugin files](processors/plugin-files.md)).

```sh
dh-waypoint plugin list                      # what is registered
dh-waypoint plugin validate my.plugin.yml    # check a file before deploying
dh-waypoint plugin test http://svc:8090      # probe a service manifest+status
```

The operator API does the same over HTTP:

```sh
# status of every registration
curl -s $B/api/processors/v1/status -H "Authorization: Bearer $PROCESSORS_ADMIN_TOKEN"

# register (generation 0 creates; see operator-api.md for updates)
curl -s -X PUT $B/api/processors/v1/registry/my-filter \
  -H "Authorization: Bearer $PROCESSORS_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' -d '{
    "expected_generation": 0,
    "driver": "builtin/filter-list",
    "endpoint": "", "mode": "shadow", "state": "active", "priority": 10,
    "configuration": {"block_patterns": ["(?i)\\bcam\\b"], "reason": "cam rips"},
    "auth_env": ""
  }'
```

Contract and field semantics: [operator API](processors/operator-api.md)
and [processor protocol v1](processors/v1.md). Every plugin starts
in `shadow` in the templates — proposals are recorded but the
catalogue is untouched until you switch to `apply`.

## 5. Import existing records

`POST /import` accepts newline-delimited JSON, one torrent per line
(`infoHash`, `name`, `size`, optional content fields), and streams
back a count. Imported rows pass the same CSAM/blocklist gates as
crawled ones. Treat this endpoint as trusted-network only.

## Building a UI or client on top

- Read path: GraphQL, REST (`/api/v1`) and Torznab are the stable
  contracts. Generate client types from GraphQL introspection or
  `/api/v1/openapi.yaml`; do not scrape metrics for catalogue data.
- Auth is yours to add: put Caddy/Traefik/NPM in front and keep
  `:3333` on a trusted network. The recipes are in
  [operations/security](operations/security.md).
- Live updates: poll GraphQL/`/metrics`, or use the GraphQL
  websocket transport the endpoint already speaks.
- Versioning: the processor contract is versioned
  (`dh-waypoint.processor.v1`); GraphQL fields are additive — pin your
  queries, not the whole schema.
