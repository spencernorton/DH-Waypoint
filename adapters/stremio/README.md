# Stremio addon bridge

A standalone Stremio Addon Protocol v1 service backed by DH-Waypoint's Torznab API. It imports only Go's standard library and runs as an unprivileged sidecar.

```sh
go build -o dh-waypoint-stremio ./adapters/stremio/cmd/dh-waypoint-stremio
DH_WAYPOINT_TORZNAB_URL=http://127.0.0.1:3333 ./dh-waypoint-stremio
```

Configuration variables:

- `STREMIO_LISTEN` (default `127.0.0.1:7000`): HTTP listen address for `/manifest.json` and `/stream/{type}/{id}.json`.
- `DH_WAYPOINT_TORZNAB_URL` (default `http://127.0.0.1:3333`): Base URL of the DH-Waypoint Torznab service.
- `DH_WAYPOINT_TORZNAB_KEY` (optional): Torznab API key passed as `apikey`.
- `STREMIO_ADDON_NAME` (default `DH-Waypoint`): Addon badge name shown in Stremio stream cards.
- `STREMIO_MAX_STREAMS` (default `25`): Maximum streams requested per IMDb lookup.
