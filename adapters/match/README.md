# DH-Waypoint Reference Metadata Match Adapter

Standalone `dh-waypoint.processor.v1` sidecar providing deterministic `extract` + `match` identity resolution against four community metadata catalogues without importing any core backend packages:

- **`anilist`** — Anime titles (`https://graphql.anilist.co`, returning `anilist:<id>` plus `mal:<id>` cross-reference evidence)
- **`musicbrainz`** — Music release groups (`https://musicbrainz.org/ws/2/release-group`)
- **`igdb`** — Video games (`https://api.igdb.com/v4/games`, requires `MATCH_CLIENT_ID` and `MATCH_API_TOKEN`)
- **`openlibrary`** — Books and audiobooks (`https://openlibrary.org/search.json`)

## Configuration

- `MATCH_PROVIDER`: `anilist` (default), `musicbrainz`, `igdb`, or `openlibrary`
- `PROCESSOR_LISTEN`: listen address (default `127.0.0.1:8097`; non-loopback requires `PROCESSOR_TOKEN`)
- `PROCESSOR_TOKEN`: bearer token checked in constant time on every `/v1/*` request
- `MATCH_ENDPOINT`: optional custom API endpoint URL
- `MATCH_MIN_CONFIDENCE`: minimum match confidence in `[0, 1]` before proposing a match (default `0.65`)
- `MATCH_USER_AGENT`: custom `User-Agent` header for MusicBrainz / OpenLibrary
- `MATCH_CLIENT_ID` / `MATCH_API_TOKEN`: Twitch/IGDB credentials when `MATCH_PROVIDER=igdb`

## Example Plugin Manifests (`plugins/*.plugin.yml`)

```yaml
enabled: true
id: match-anilist
endpoint: http://127.0.0.1:8097
mode: observe
capabilities:
  - extract
  - match
selector:
  is_anime: true
  has_content_match: false
```

```yaml
enabled: true
id: match-musicbrainz
endpoint: http://127.0.0.1:8098
mode: observe
capabilities:
  - extract
  - match
selector:
  content_types:
    - music
  has_content_match: false
```

Verify before enabling `mode: apply`:

```sh
dh-waypoint plugin verify http://127.0.0.1:8097
```
