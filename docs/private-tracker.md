# Private torrents and the built-in tracker

DH-Waypoint can run a small private library alongside its public DHT
index: create private `.torrent` files, upload them to the core,
hand members personalised copies, and track their swarms with a
built-in HTTP tracker. Private torrents are never served through
GraphQL or Torznab, never scraped against public trackers, and
never sent to processor plugins — the private API below is the
only way in or out.

What this is not: a tracker *site*. There are no accounts, ratio
economy, invites, forums, or browse pages in core. Passkeys gate
the tracker, one admin token gates management, and anything richer
belongs to a site layer in front of the API.

## Enable it

```sh
TRACKER_ENABLED=true
TRACKER_PASSKEYS=member-one-key,member-two-key   # one per member/client
TRACKER_ADMIN_TOKEN=<openssl rand -hex 32>
TRACKER_PUBLIC_BASE_URL=https://tracker.example.com   # reachable by clients
```

Every value also accepts the `_FILE` secret form. Announce/scrape
answer only when `TRACKER_ENABLED` is set and at least one passkey
exists; the file API answers when `TRACKER_ADMIN_TOKEN` is set
(independent of the tracker, so an external tracker works too).
Verify with `dh-waypoint config show | grep tracker`.

## Create a private torrent

```sh
dh-waypoint torrent create --private \
  --tracker https://tracker.example.com/announce/member-one-key \
  ./My.Release.2026
# wrote My.Release.2026.torrent
# info_hash: 0123abcd...
```

Piece length is automatic by size (`--piece-length` overrides).
`--source` sets the BEP-27 source field. The created file is
re-validated through the core's own metainfo parser before it is
written. Seed this exact file from your client — for private
torrents the info dictionary (including the private flag) is the
identity, and clients must keep DHT/PEX/LSD off for it, which
mainstream clients do automatically when the flag is set.

## Upload it to the core

```sh
curl -X POST http://localhost:3333/api/private/v1/torrents \
  -H "Authorization: Bearer $TRACKER_ADMIN_TOKEN" \
  --data-binary @My.Release.2026.torrent
# 201 {"info_hash":"0123...","name":"My.Release.2026","private":true,"existed":false}
```

Uploads pass the same ingestion gates as crawled torrents (CSAM
blocklist, blocking, name policy) and enqueue normal core
processing under your `PROCESSING_MODE`. The exact file bytes are
stored for download; re-uploading refreshes them. Torrent files
created elsewhere (qBittorrent, libtorrent, another tracker) upload
the same way — public files included; `private` simply reflects
the BEP-27 flag inside the file.

## Members download personalised copies

```sh
curl -O "http://localhost:3333/api/private/v1/torrents/<info_hash>/download?passkey=member-two-key"
```

With a valid passkey, the announce URL is rewritten to
`<public base>/announce/<that passkey>` — outside the info
dictionary, so every member's copy shares one info hash and one
swarm. With the admin token instead of a passkey, the stored file
is served verbatim. Details for your own member UI:

```sh
curl http://localhost:3333/api/private/v1/torrents/<info_hash> \
  -H "Authorization: Bearer $TRACKER_ADMIN_TOKEN"
# {"info_hash":"...","name":"...","private":true,"files":[...],
#  "seeders":2,"leechers":1,"completed":5}
```

## How the tracker behaves

- Endpoints: `GET /announce/<passkey>`, `GET /scrape/<passkey>`
  (BEP-3 HTTP tracker semantics; BEP-23 compact peers including
  `peers6`; scrape accepts repeated `info_hash` parameters).
- Only torrents already in the catalogue (uploaded or crawled)
  can announce; unknown hashes get a bencoded
  `unregistered torrent` failure, bad passkeys `invalid passkey`.
- Peers expire after two announce intervals without contact
  (`TRACKER_ANNOUNCE_INTERVAL`, default 30m). `downloaded` in
  scrape is an all-time per-peer completion total (each peer
  identity counted once, surviving peer-row retention).
- The tracker is HTTP-only. Mainstream clients speak it natively;
  a UDP (BEP-15 server) endpoint is deliberately out of scope.
- Client IP comes from the connection (respect your reverse
  proxy's forwarded headers the same way as the rest of the HTTP
  server); the `ip` announce parameter is only a fallback and is
  never an authentication input.

Rotate a member by removing their passkey and restarting the
core; their personalised files stop announcing at the next
interval. There is no per-passkey accounting beyond peer
presence — if you need ratio enforcement, front the tracker with
your own service and keep DH-Waypoint as the catalogue.
