# Independent LLM processor

Build and run this service separately from DH-Waypoint:

```sh
go build -o dh-waypoint-llm ./adapters/llm/cmd/dh-waypoint-llm
./dh-waypoint-llm -config adapters/llm/local.example.json
```

The local and hosted examples disable inference. [`openrouter.json`](openrouter.json)
enables it for OpenRouter and is what the root `docker-compose.yml` runs under
the `openrouter` profile ([three steps](../../docs/processors/llm.md#openrouter-in-three-steps)).
The backend does not import this package.
The service imports Go's standard library and the public processor contract;
it needs no backend database, worker, private service or model library.

See [setup, inference API, budgets and migration](../../docs/processors/llm.md).
To build its own image, use the repository root as context:

```sh
docker build -f adapters/llm/Dockerfile -t dh-waypoint-llm:local .
```
