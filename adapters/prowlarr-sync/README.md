# Prowlarr / Torznab feed sync sidecar

Polls an external Torznab or Prowlarr RSS endpoint and forwards newly discovered infohashes to DH-Waypoint's fault-isolated `POST /import?on_error=skip` endpoint. Imports only Go's standard library.

```sh
go build -o dh-waypoint-prowlarr-sync ./adapters/prowlarr-sync/cmd/dh-waypoint-prowlarr-sync
PROWLARR_TORZNAB_FEED_URL="http://127.0.0.1:9696/1/api?t=search" ./dh-waypoint-prowlarr-sync -interval 0
```

Configuration variables:

- `PROWLARR_TORZNAB_FEED_URL`: Required Torznab/Newznab RSS URL to poll.
- `PROWLARR_API_KEY` (optional): API key appended as `apikey` when querying the feed.
- `DH_WAYPOINT_URL` (default `http://127.0.0.1:3333`): Base URL of the DH-Waypoint `/import` server.
- `PROWLARR_IMPORT_SOURCE` (default `prowlarr-sync`): Sent in `X-Import-Id` and item `source`.
