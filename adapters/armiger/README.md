# Armiger processor bridge

A standalone `dh-waypoint.processor.v1` service that answers `classify` jobs by
calling an [Armiger Ultra](https://github.com/spencernorton/armiger) container's
`POST /v1/classify`. The backend does not import it; it imports only Go's
standard library and the public processor contract.

The root `docker-compose.yml` runs it under the `armiger` profile; see
[the integration guide](../../docs/armiger-integration.md). To run it yourself:

```sh
go build -o dh-waypoint-armiger ./adapters/armiger/cmd/dh-waypoint-armiger
ARMIGER_URL=http://127.0.0.1:8096 ./dh-waypoint-armiger
```

| Variable | Default | Meaning |
|---|---|---|
| `PROCESSOR_LISTEN` | `127.0.0.1:8095` | Address the backend calls |
| `ARMIGER_URL` | `http://armiger-ultra:8096` | Base URL of the Armiger container |
| `ARMIGER_VERSION` | `unknown` | Reported as the manifest model version |
| `ARMIGER_MIN_CONFIDENCE` | `0.6` | Category confidence below which the bridge abstains |
| `PROCESSOR_TOKEN` | *(empty)* | Bearer token the backend must send; required for a non-loopback listen address |

To build its image, use the repository root as context:

```sh
docker build -f adapters/armiger/Dockerfile -t dh-waypoint-armiger:local .
```
