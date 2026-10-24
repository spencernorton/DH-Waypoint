# Independent LLM processor

Build and run this service separately from DH-Waypoint:

```sh
go build -o dh-waypoint-llm ./plugins/llm/cmd/dh-waypoint-llm
./dh-waypoint-llm -config plugins/llm/local.example.json
```

Both examples disable inference. The backend does not import this package.
The service imports Go's standard library and the public processor contract;
it needs no backend database, worker, private service or model library.

See [setup, inference API, budgets and migration](../../docs/processors/llm.md).
To build its own image, use the repository root as context:

```sh
docker build -f plugins/llm/Dockerfile -t dh-waypoint-llm:local .
```
