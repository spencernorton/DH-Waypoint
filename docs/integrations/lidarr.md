# Lidarr integration

DH-Waypoint's Torznab caps declare music-search support; Lidarr consumes it as a Custom Torznab indexer.

## Setup

1. **Open Lidarr → Settings → Indexers → Add → Torznab → Custom**.
2. Fill in the fields:

   | Field | Value |
   |---|---|
   | Name | `DH-Waypoint` |
   | Enable | ✓ |
   | URL | `http://dh-waypoint:3333/torznab` (compose) or `https://dh-waypoint.example.com/torznab` (public) |
   | API Key | your `TORZNAB_API_KEY` |
   | Categories | `3000` (Audio), `3030` (Audio/Audiobook — optional) |
   | Search modes | `q` only (from caps; `artist` and `album` are not supported) |
   | Priority | `25–50` |
   | Early Download Limit | leave default (Lidarr's value) |

3. Click **Test**. Expect green *"Test was successful"*.
4. Save.

`[screenshot: lidarr-add-indexer-form]`

## Search modes

DH-Waypoint's caps always advertise `music-search` with `q` as its only parameter and mark `audio-search` unavailable, so Lidarr searches with free text (`q`) rather than discrete `artist`/`album` parameters. `t=music`, like `cat=3000`, matches only torrents typed `music`.

`[screenshot: lidarr-indexer-test-success]`

## Categories reference

| Newznab ID | Lidarr label | Notes |
|---|---|---|
| 3000 | Audio | music |
| 3030 | Audio/Audiobook | optional; also handled by Readarr |

`[screenshot: lidarr-categories-selector]`

## Troubleshooting

**0 results on a music search**
Verify by hand: `curl "https://dh-waypoint.example.com/torznab/api?t=music&q=Pink+Floyd+Animals&apikey=$KEY"`. If the XML returns 0 items, DH-Waypoint hasn't crawled that release. If items return but Lidarr still shows 0, it's a category-filter mismatch — check that 3000 is in your indexer config.

`[screenshot: lidarr-custom-formats]`

## Recommended companions

- **MusicBrainz integration** — Lidarr resolves artist/album metadata via MusicBrainz before querying indexers; DH-Waypoint doesn't change this flow.
- **Quality Profiles** — start with Lidarr's defaults; tune later as you see what DH-Waypoint surfaces.
- **Search delays** — leave default; DH-Waypoint's classifier finishes processing within seconds of DHT discovery.
