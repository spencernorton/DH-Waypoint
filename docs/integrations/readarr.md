# Readarr integration

Readarr is the ebook + audiobook member of the `*arr` family. DH-Waypoint answers both Readarr's book and audiobook search calls — books map to the Newznab 7000-series categories, audiobooks to category `3030`. A single DH-Waypoint indexer entry per Readarr instance is sufficient.

## Setup

1. **Open Readarr → Settings → Indexers → Add → Torznab → Custom**.
2. Fill in the connection fields:

   | Field | Value |
   |---|---|
   | Name | `DH-Waypoint` |
   | Enable | ✓ |
   | URL | `http://dh-waypoint:3333/torznab/api` (compose service name) or `https://dh-waypoint.example.com/torznab/api` (public host) |
   | API Key | your `TORZNAB_API_KEY` |
   | Categories | `7000` Books, `7020` EBook, `7030` Comics — pick what your library tracks |
   | Early Download Limit | leave default |
   | Priority | `25` (high) to `50` (medium-fallback) — tune per your stack |
   | Tags | optional |

3. Click **Test**. The button turns green: *"Test was successful"*.
4. Save.

For a separate Readarr **audiobook** instance, repeat the setup with category `3030` selected instead.

## Search modes

DH-Waypoint's caps always advertise `book-search` with `q` as its only parameter and mark `audio-search` unavailable (`t=audio` returns error `202`), so Readarr searches ebooks and audiobooks with free text (`q`) rather than `author`/`title` parameters. `t=book` matches ebooks, comics and audiobooks; the categories on the indexer entry narrow the results.

## Categories reference

| Newznab ID | Readarr label | Notes |
|---|---|---|
| 3030 | Audio / Audiobook | use for audiobook instances |
| 7000 | Books | catch-all: ebooks, comics and audiobooks |
| 7020 | Books / EBook | same filter as 7000 |
| 7030 | Books / Comics | comics only |

If your Readarr instance only tracks audiobooks, leave the 7000-series boxes unchecked and pick `3030` only — this keeps Readarr's indexer cache lean.

## Troubleshooting

**Test returns "Connection failed"**
The URL field is referencing `localhost`, which from inside Readarr's container resolves to Readarr itself, not DH-Waypoint. Use the **compose service name** (`dh-waypoint`) or the **public host** — both are reachable from Readarr's network.

**Test returns 401**
The API Key field does not match `TORZNAB_API_KEY`. Common causes: trailing whitespace pasted from a `.env` file, surrounding quotes, a stale value left over from a previous DH-Waypoint install.

**Search returns 0 results**
Inspect crawl metrics and the [GraphQL catalog](../reference/graphql-api.md). If no torrents have been discovered, review [troubleshooting](../troubleshooting.md). If the catalog contains torrents but Readarr's search misses, try the same title as `q` manually via curl against `/torznab/api?t=book&q=...`. If curl returns results and Readarr does not, the issue is most likely a Readarr category-map mismatch.

**Audiobook search returns ebooks (or vice versa)**
The category map on the DH-Waypoint indexer entry is wrong for the Readarr instance type. For an audiobook instance, select only `3030`. DH-Waypoint's `7000` and `7020` filters also match audiobooks (returned as `3030`), so an ebook instance can still receive them.

## Recommended companions

- **Quality Profiles** — leave standard.
- **Indexer flags** — flag DH-Waypoint as a fallback indexer for hard-to-find titles. Private trackers handle the headline catalog; DH-Waypoint extends reach into older or less-popular titles that aren't on private indexers.

For multi-instance Readarr setups (typical: one ebook instance + one audiobook instance), repeat the setup in each — DH-Waypoint does not require any special multi-tenant configuration.

## See also

- [Torznab API reference](../reference/torznab-api.md)
- [Sonarr integration](sonarr.md) — the same patterns apply for TV
- [Lidarr integration](lidarr.md) — sister `*arr` for music
- [Configuration](../configuration.md) — `TORZNAB_API_KEY` setup
