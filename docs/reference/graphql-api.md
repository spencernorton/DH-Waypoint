# GraphQL API reference

DH-Waypoint exposes a GraphQL endpoint at `:3333/graphql`. The schema is defined in `graphql/schema/*.graphqls`, served with [gqlgen](https://github.com/99designs/gqlgen) (`internal/gql/gqlgen.yml`), and supports full introspection.

## Overview

| Property | Value |
|---|---|
| Endpoint | `POST http://<host>:3333/graphql` |
| Browser page | none; `GET /graphql` returns `404` |
| Auth | `HTTP_SERVER_API_KEY` when set (`Authorization: Bearer` or `X-API-Key`); open when unset |
| Schema introspection | enabled |

Use curl or a separate GraphQL client with JSON POST requests.

Requests require `Content-Type: application/json`; multipart forms and WebSocket
upgrades are unsupported. The endpoint has a 1 MiB body limit, a 64 KiB query
document limit and a 10,000-token parser limit. Operations allow at most 1,000
expanded selections, 20 levels of selection/fragment nesting and 32 fields on
query or mutation service objects. Repeated fragment spreads count toward the
same budget, including for cached and persisted operations.

Paged inputs accept `limit` from 0 to 1,000. Input lists contain at most 1,000
entries, `queryString` is at most 4,096 bytes and an explicit
`aggregationBudget` is from 1 to 5,000 planner-cost units. Split larger bulk
operations and paginate results. Rejected operations return an error with code
`OPERATION_LIMIT_EXCEEDED` before any resolver runs; other parser/schema failures
retain their normal GraphQL errors. An oversized body returns HTTP 413 and an
unsupported content type returns HTTP 415.

GraphQL and REST v1 catalogue requests give downstream work a 30-second context
deadline, with up to 10 seconds to read the body and 5 additional seconds to
write the response. Disconnects and earlier client-context deadlines still
cancel cooperative database work. These are per-request limits; operators
should also bound concurrent requests at their proxy. The separate streaming
import endpoint retains its streaming behavior.

## Top-level Query fields

| Field | Type | Purpose |
|---|---|---|
| `version` | `String` | Version of the running build |
| `torrent` | `TorrentQuery` | Torrent-centric reads — files, sources, metrics, tag suggestions |
| `torrentContent` | `TorrentContentQuery` | Search across enriched content (movies / TV / music / books) |
| `queue` | `QueueQuery` | Queue / job state |
| `workers` | `WorkersQuery` | Worker registry |
| `health` | `HealthQuery` | Overall `status` plus per-component `checks` |
| `evidence` | `EvidenceQuery` | `*arr` grab evidence and per-indexer stats |

### `TorrentQuery` fields

| Field | Returns | Purpose |
|---|---|---|
| `files(input)` | `TorrentFilesQueryResult` | List files for given infohashes |
| `listSources()` | `TorrentListSourcesResult` | Distinct torrent sources known to the indexer |
| `metrics(input)` | `TorrentMetricsQueryResult` | Bucketed counts over time |
| `suggestTags(input)` | `TorrentSuggestTagsResult` | Tag suggestions for filtering |

### `TorrentContentQuery` fields

| Field | Returns | Purpose |
|---|---|---|
| `search(input: TorrentContentSearchQueryInput)` | `TorrentContentSearchResult` | Primary search |

`TorrentContentSearchResult` shape:

| Field | Purpose |
|---|---|
| `items` | The matched torrent-content rows |
| `totalCount` | Estimated or exact count when the input sets `totalCount: true` (otherwise `0`), see next field |
| `totalCountIsEstimate` | If `true`, `totalCount` came from a planner estimate |
| `hasNextPage` | True when another page exists; computed only when the input sets `hasNextPage: true` |
| `aggregations` | Facet counts for each facet the input requests with `aggregate: true`: `contentType`, `genre`, `language`, `releaseYear`, `torrentFileType`, `torrentSource`, `torrentTag`, `videoResolution`, `videoSource` |

## Common queries

### Search torrents by free text

```graphql
query Search {
  torrentContent {
    search(input: { queryString: "the matrix", limit: 25, totalCount: true, hasNextPage: true }) {
      items {
        infoHash
        title
        contentType
        publishedAt
      }
      totalCount
      hasNextPage
    }
  }
}
```

### Category facet counts (no item rows)

Useful for category aggregation and cardinality probes.

```graphql
query Facets {
  torrentContent {
    search(input: {
      queryString: "matrix", limit: 0, totalCount: true
      facets: { contentType: { aggregate: true } }
    }) {
      totalCount
      aggregations {
        contentType {
          value
          label
          count
          isEstimate
        }
      }
    }
  }
}
```

### List the 10 most-recent torrents

```graphql
query MostRecent {
  torrentContent {
    search(input: { limit: 10, orderBy: { field: published_at, descending: true } }) {
      items {
        infoHash
        title
        publishedAt
      }
    }
  }
}
```

### Files for a specific infohash

```graphql
query Files {
  torrent {
    files(input: { infoHashes: ["a1b2c3d4e5f67890123456789012345678901234"] }) {
      items {
        infoHash
        path
        size
        index
        extension
        fileType
      }
    }
  }
}
```

### Health check

```graphql
query Health {
  health {
    status
    checks { key status error }
  }
}
```

### Worker list

```graphql
query Workers {
  workers {
    listAll {
      workers {
        key
        started
      }
    }
  }
}
```

## Top-level Mutation fields

| Field | Type | Purpose |
|---|---|---|
| `torrent` | `TorrentMutation` | Torrent management — delete / tag / reprocess |
| `queue` | `QueueMutation` | Queue management — purge / enqueue reprocess batches |

### `TorrentMutation` fields

| Field | Args | Returns |
|---|---|---|
| `delete` | `infoHashes: [Hash20!]!` | `Void` (currently refused; see [Error handling](#error-handling)) |
| `setTags` | `infoHashes, tagNames` | `Void` |
| `putTags` | `infoHashes, tagNames` | `Void` (additive) |
| `deleteTags` | `infoHashes, tagNames` | `Void` |
| `reprocess` | `input: TorrentReprocessInput!` (`infoHashes` required) | `Void` |

### `QueueMutation` fields

| Field | Args | Returns |
|---|---|---|
| `purgeJobs` | `input: QueuePurgeJobsInput!` | `Void` |
| `enqueueReprocessTorrentsBatch` | `input: QueueEnqueueReprocessTorrentsBatchInput` | `Void` |

## Common mutations

### Apply tags to a torrent

```graphql
mutation Tag {
  torrent {
    setTags(
      infoHashes: ["a1b2c3d4e5f67890123456789012345678901234"]
      tagNames: ["operator-curated", "trusted"]
    )
  }
}
```

### Reprocess after editing the classifier

```graphql
mutation Reprocess {
  torrent {
    reprocess(input: { infoHashes: ["a1b2c3d4e5f67890123456789012345678901234"], classifierRematch: true })
  }
}
```

(For bulk reprocess across the whole corpus, use the `dh-waypoint reprocess` CLI command instead.)

## Custom scalars

| Scalar | Description |
|---|---|
| `Hash20` | 40-character hex info_hash |
| `DateTime` | RFC3339 |
| `Date` | `YYYY-MM-DD` |
| `Year` | Integer year |
| `Duration` | ISO 8601 duration |
| `Void` | Mutation result, always `null`; failures come back in `errors[]` |
| `ContentType` | Enum: `movie`, `tv_show`, `music`, `ebook`, `comic`, `audiobook`, `game`, `software`, `xxx`, `course` |
| `FileType` | Enum: `archive`, `audio`, `data`, `document`, `image`, `software`, `subtitles`, `video` |
| `VideoResolution` | Enum: `V360p`, `V480p`, `V540p`, `V576p`, `V720p`, `V1080p`, `V1440p`, `V2160p`, `V4320p` |
| `VideoSource` | Enum: `CAM`, `TELESYNC`, `TELECINE`, `WORKPRINT`, `DVD`, `TV`, `WEBDL`, `WEBRip`, `BluRay` |
| `VideoCodec` | Enum: `H264`, `x264`, `x265`, `XviD`, `DivX`, `MPEG2`, `MPEG4`, `HEVC`, `AV1` |
| `Video3D`, `VideoModifier` | Enums: `V3D`, `V3DSBS`, `V3DOU`; `REGIONAL`, `SCREENER`, `RAWHD`, `BRDISK`, `REMUX` |
| `LanguageInfo` | Object: `id`, `name` |

## Pagination

Set `input.offset`, `input.limit` and `input.hasNextPage: true`. The result's `hasNextPage` is then true when more rows exist; loop until it goes false.

```graphql
query Page {
  torrentContent {
    search(input: { queryString: "ubuntu", limit: 50, offset: 100, hasNextPage: true }) {
      items { infoHash title }
      hasNextPage
    }
  }
}
```

A `limit` above 1,000 is rejected with `OPERATION_LIMIT_EXCEEDED`.

## Error handling

GraphQL errors come back in the standard `errors[]` array. For example, `torrent.delete` currently refuses every hash because [catalogue recovery](../catalogue-recovery.md) is disabled:

```json
{
  "data": { "torrent": { "delete": null } },
  "errors": [
    {
      "message": "catalogue recovery: foundation disabled",
      "path": ["torrent", "delete"]
    }
  ]
}
```

Field-level errors return `data` with the partial result and a non-empty `errors[]`.

## See also

- [Torznab API reference](torznab-api.md)
- [CLI reference](cli.md) — some GraphQL operations have a CLI equivalent (`reprocess`)
- [Configuration](../configuration.md)
