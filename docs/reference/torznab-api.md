# Torznab API Reference

DH-Waypoint exposes a Newznab/Torznab-compatible feed at `/torznab/api`. The adapter follows the [Torznab spec 1.3 draft](https://torznab.github.io/spec-1.3-draft/), which is itself an extension of the Newznab API.

This page is the user-facing reference. For per-`*arr` setup walkthroughs see the [Integrations](../integrations/sonarr.md) section.

## Overview

| Property | Value |
|---|---|
| Base URL | `http://<host>:3333/torznab/api` |
| Auth | `apikey=<key>` query parameter or `X-Api-Key` header (required when `TORZNAB_API_KEY` or `TORZNAB_API_KEYS` is set) |
| Response format | RSS 2.0 XML with `<torznab:attr>` extensions |
| Spec source | <https://torznab.github.io/spec-1.3-draft/external/newznab/api.html> |

## Authentication

DH-Waypoint gates `/torznab/api` on the `TORZNAB_API_KEY` environment variable and on `TORZNAB_API_KEYS`, a comma-separated list of named `name:key` pairs.

- **Neither set** — endpoint is open. Use only on a tailnet or behind a reverse proxy that adds its own auth.
- **Either set** — every request must carry a matching key as the `apikey` query parameter or the `X-Api-Key` header. The compare is constant-time (timing-safe).

A failed auth returns Newznab error code `100` (`Incorrect user credentials`) with HTTP `401`:

```xml
<error error="100" description="Incorrect user credentials"/>
```

## Functions (`t=`)

The Torznab function is selected with the `t` query parameter.

| `t` value | Purpose |
|---|---|
| `caps` | Capabilities document — supported functions, categories, attributes |
| `search` | Generic free-text search |
| `tvsearch` | TV episode/season search |
| `movie` | Movie search |
| `music` | Music album/artist search |
| `book` | Book / audiobook search |

## Search parameters

Each function accepts a different parameter set.

| Function | Common params | Identifier params |
|---|---|---|
| `search` | `q`, `cat`, `limit`, `offset` | `imdbid`, `tmdbid` |
| `tvsearch` | `q`, `season`, `ep`, `cat`, `limit`, `offset` | `tvdbid`, `imdbid`, `tmdbid` |
| `movie` | `q`, `cat`, `limit`, `offset` | `imdbid`, `tmdbid` |
| `music` | `q`, `cat`, `limit`, `offset` | — |
| `book` | `q`, `cat`, `limit`, `offset` | — |

Notes:

- `cat` is a comma-separated list of Newznab category IDs.
- The default profile returns at most 100 items, with a default limit of 100.
  Other profiles advertise their own limits in `t=caps`.
- `offset` is for pagination.
- Multiple supplied identifiers are alternatives (OR), following the
  [supportedParams revision](https://torznab.github.io/spec-1.3-draft/revisions/1.1-Newznab-supportedParams.html).
  A missing IMDb mapping therefore does not veto a known TMDB or TVDB mapping.
  The function's content type, categories, episode/date and any `q` text still
  constrain those matches. Movie and TV numeric IDs occupy separate namespaces.
- ID queries use accepted canonical identities and catalogue attributes whose
  key is `id`. Plugin proposals, alternate titles and unverified annotations
  cannot establish an ID or grant serving eligibility.
- An unmapped torrent remains available to ordinary text search. An ID-only
  request cannot prove its identity; clients can use their text-search fallback.
  Search does not call a classifier or depend on a plugin being online.
- Category subtypes can constrain resolution. There is no generic `quality=`
  parameter; clients also evaluate release names and returned attributes.
  `tvmazeid`, `traktid`, `artist`, `album`, `author` and `title` are not advertised
  or implemented search parameters.

## Newznab categories

DH-Waypoint advertises only these Newznab categories in `t=caps` (`internal/torznab/categories.gen.go`):

| ID | Category | Subcategories |
|---|---|---|
| 2000 | Movies | 2030 SD, 2040 HD, 2045 UHD, 2060 3D |
| 3000 | Audio | 3030 Audiobook |
| 4000 | PC | 4050 Games |
| 5000 | TV | 5030 SD, 5040 HD, 5045 UHD, 5070 Anime |
| 6000 | XXX | 6070 Other |
| 7000 | Books | 7020 EBook, 7030 Comics |
| 8000 | Other | |

Each result carries the category of its content type: 2000 movie, 5000 TV (anime TV also 5070), 3000 music, 3030 audiobook, 7000 ebook, 7030 comic, 4000 software, 4050 game, 6000 XXX, and 8000 anything else (`internal/torznab/adapter/search_result.go`).

## Response format

Successful responses are RSS 2.0 XML with `<torznab:attr name="..." value="..."/>` per-item attributes.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>DH-Waypoint</title>
    <item>
      <title>Example Release Title (2025) 1080p WEB-DL</title>
      <guid>{infohash}</guid>
      <pubDate>Tue, 28 Apr 2026 12:00:00 +0000</pubDate>
      <category>movie</category>
      <size>123456789</size>
      <enclosure url="magnet:?xt=urn:btih:..." length="123456789" type="application/x-bittorrent;x-scheme-handler/magnet"/>
      <torznab:attr name="infohash" value="..."/>
      <torznab:attr name="magneturl" value="magnet:?xt=urn:btih:..."/>
      <torznab:attr name="category" value="2000"/>
      <torznab:attr name="size" value="123456789"/>
      <torznab:attr name="publishdate" value="Tue, 28 Apr 2026 12:00:00 +0000"/>
      <torznab:attr name="seeders" value="142"/>
      <torznab:attr name="leechers" value="16"/>
      <torznab:attr name="peers" value="158"/>
      <torznab:attr name="resolution" value="1080p"/>
    </item>
  </channel>
</rss>
```

The attributes emitted per item (`internal/torznab/adapter/search_result.go`):

| Attribute | Emitted |
|---|---|
| `infohash`, `magneturl`, `category`, `size`, `publishdate` | always; anime TV adds a second `category` of `5070` |
| `seeders` | when known; a DHT-only zero is omitted while `TORZNAB_ZERO_SEEDERS_AUTHORITATIVE_ONLY` is `true` |
| `seedscheckedat` | when a tracker scrape verified the counts (RFC 3339) |
| `leechers`, `peers` | when known; `peers` (seeders + leechers) only with `seeders` |
| `englishaudio` | anime: `dub`, `sub` or `none` |
| `season`, `episode`, `video`, `resolution`, `team` | when parsed; `video` is the codec, `team` the release group |
| `claimedhdr`, `claimedaudio`, `claimedaudiochannels`, `claimedaudiofeatures`, `claimedrevision`, `claimedencoder` | release-name claims, when present |
| `year`, `tmdb`, `tmdbid`, `tvdbid` (TV only), `imdb` (without `tt`) | the matched title's release year and catalogue IDs |

## Result filters

Operator-controlled filters trim dead-confidence items from search responses before they reach `*arr`. They run after the [liveness blocklist](../evidence.md) and have no DB round trip — pure CPU on the in-memory result set.

| Env var | Default | Effect |
|---|---|---|
| `TORZNAB_HIDE_ZERO_SEEDERS` | `true` | Drop items with an authoritative zero-seeder verdict. *arrs treat seeders=0 as "queue it anyway" and the resulting grab sits in metaDL forever. Without this filter, those releases burn one operator-action cycle (queue → metaDL → reaped) for guaranteed-dead torrents. |
| `TORZNAB_HIDE_UNKNOWN_SEEDERS_AGE_DAYS` | `7` | Drop items with no trusted seeder data AND every source row older than the threshold. Catches stale DHT-only discoveries that nobody has re-confirmed recently. Set `0` to disable. Fresh DHT discoveries are still surfaced — recency *is* signal. |
| `TORZNAB_ZERO_SEEDERS_AUTHORITATIVE_ONLY` | `true` | Defines what counts as a real 0 for both filters above. When `true`, only the `tracker` source row (mirrored from the tracker-scrape ledger (`internal/seeds`, `torrent_tracker_seeds`)) is authoritative: a DHT BEP-33 bloom-approximated 0 — which many young, alive swarms report at crawl time — is treated as honest-unknown instead: served while any source is recent, aged out by the unknown-seeders window, and reported **without** a `seeders` attr so `*arr` minimumSeeders treats it as unknown rather than rejecting it. Set `false` for the pre-v0.41 behavior, where any scraped 0 (including the bloom approximation) hides the item and is reported as `0`. |

The filters increment the same metric as the liveness filter (`dh_waypoint_liveness_torznab_excluded_total`).

## Error codes

Errors follow Newznab error semantics. DH-Waypoint emits these codes:

| Code | Meaning |
|---|---|
| 100 | Incorrect user credentials (HTTP `401`) |
| 200 | Missing parameter: no `t` |
| 202 | No such function: an unknown `t`, such as `t=audio` |

An unknown profile path or a failed search returns a plain-text HTTP `404` or `500` instead.

Error response shape:

```xml
<error error="200" description="missing parameter (t)"/>
```

## Examples

Set the API key once for these examples:

```bash
export TORZNAB_API_KEY=<your_key>
```

**Capabilities:**

```bash
curl -s "http://localhost:3333/torznab/api?t=caps&apikey=$TORZNAB_API_KEY"
```

**TV episode by tvdbid (Game of Thrones, S01):**

```bash
curl -s "http://localhost:3333/torznab/api?t=tvsearch&tvdbid=121361&season=1&apikey=$TORZNAB_API_KEY"
```

**TV episode with explicit season + episode:**

```bash
curl -s "http://localhost:3333/torznab/api?t=tvsearch&q=breaking%20bad&season=5&ep=14&apikey=$TORZNAB_API_KEY"
```

**Movie by IMDb ID (Interstellar):**

```bash
curl -s "http://localhost:3333/torznab/api?t=movie&imdbid=tt0816692&apikey=$TORZNAB_API_KEY"
```

**Movie by TMDB ID:**

```bash
curl -s "http://localhost:3333/torznab/api?t=movie&tmdbid=157336&apikey=$TORZNAB_API_KEY"
```

**Music album:**

```bash
curl -s "http://localhost:3333/torznab/api?t=music&q=Radiohead+In+Rainbows&apikey=$TORZNAB_API_KEY"
```

**Book / audiobook:**

```bash
curl -s "http://localhost:3333/torznab/api?t=book&q=Brandon+Sanderson+Mistborn&apikey=$TORZNAB_API_KEY"
```

**Free-text search with category filter:**

```bash
curl -s "http://localhost:3333/torznab/api?t=search&q=ubuntu&cat=4000&apikey=$TORZNAB_API_KEY"
```

## Limits and rate-limiting

DH-Waypoint does not impose internal rate limits on `/torznab/api`. Concurrency is bounded only by Postgres connection pool size and CPU. For public-internet exposure, place a reverse proxy (Caddy, Traefik, NPM) in front and configure rate-limiting there.

The `*arr` clients respect Torznab `Retry-After` headers; you can shed load by returning `429` from your reverse proxy.

## Discovery

There is no Torznab service-discovery endpoint. Clients are expected to be configured with the base URL and API key directly.

## See also

- [Sonarr integration](../integrations/sonarr.md)
- [Radarr integration](../integrations/radarr.md)
- [Prowlarr integration](../integrations/prowlarr.md)
- [Lidarr integration](../integrations/lidarr.md)
- [Readarr integration](../integrations/readarr.md)
- [Configuration](../configuration.md)
- [GraphQL API reference](graphql-api.md)
