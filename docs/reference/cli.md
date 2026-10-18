# CLI reference

The DH-Waypoint binary is named `dh-waypoint`. Commands are sorted alphabetically by the underlying `urfave/cli` runtime. Run with no arguments — or `--help` — for the live command list:

```bash
dh-waypoint --help
```

The command set is small and stable. Anything not on this page does not exist.

## Top-level commands

| Command | Purpose |
|---|---|
| `worker run` | Start one or more workers and block (the container default) |
| `worker list` | Print the registered worker keys |
| `classifier show` | Print the currently loaded classifier workflow source |
| `classifier schema` | Print the JSON schema describing the workflow source |
| `config show` | Render every resolved config path with values, defaults, and source |
| `init` | Scaffold `config.yml`, `.env` with generated secrets, and a `plugins/` directory |
| `doctor` | Check processing mode, database TCP reachability, and plugin files |
| `plugin list` | List registered processor plugins |
| `plugin validate` | Validate a `*.plugin.yml` file without registering it |
| `plugin test` | Probe a processor service manifest and status |
| `torrent create` | Hash a file or directory into a `.torrent` file (`--private`, `--tracker`, `--piece-length`) |
| `process` | One-shot: run the classification + persistence pipeline once over enqueued items, then exit |
| `reprocess` | Re-classify already-indexed torrents through the current classifier |
| `attribution` | Attribution sub-commands (powers wantbridge + `*arr` grab attribution) |
| `migrate` | (dev) database migrations — wraps goose; the worker auto-migrates on startup |
| `gorm` | (dev) GORM tooling |

## `dh-waypoint worker`

The entry point for running DH-Waypoint. The container's default command is `worker run --all`.

### `worker run`

```
worker run [--all] [--keys k1,k2,...]
```

| Flag | Purpose |
|---|---|
| `--all` | Enable every registered worker |
| `--keys` | Comma-separated list of worker keys (use `worker list` to discover) |

Examples:

```bash
# Production — all workers (default for the container)
dh-waypoint worker run --all

# Debug a single worker locally
dh-waypoint worker run --keys dht_crawler

# Subset
dh-waypoint worker run --keys dht_crawler,queue_server,evidence_arr_poller
```

The command blocks. SIGINT/SIGTERM stops all workers cleanly.

### `worker list`

```bash
dh-waypoint worker list
```

One key per line. Useful for confirming which workers a build registered.

## `dh-waypoint classifier`

Inspect the live classifier without restarting the worker.

### `classifier show`

```
classifier show [--format yaml|json]
```

Prints the loaded classifier workflow source — CEL rules + content-type mapping.

```bash
# Default — yaml
dh-waypoint classifier show

# Export to a file for diff'ing
dh-waypoint classifier show > current-rules.yaml

# JSON for programmatic consumers
dh-waypoint classifier show --format json | jq '.rules[0]'
```

### `classifier schema`

```
classifier schema [--format yaml|json]
```

Prints the JSON Schema describing the workflow source. Useful for IDE auto-complete and validation when you're writing/editing rules.

```bash
dh-waypoint classifier schema --format json > classifier.schema.json
```

## `dh-waypoint config show`

```bash
dh-waypoint config show
```

Renders every resolved configuration path. Output is a wide table:

| Column | Meaning |
|---|---|
| `path` | Dot-notation config key (e.g. `dht.scaling_factor`) |
| `Type` | Go type |
| `Value` | Currently resolved value |
| `Default` | Default if no override applies |
| `From` | Which resolver produced the value (env-var name, `default`, or `file`) |

The `From` column is the most useful single signal — it tells you exactly why a config has the value it does, including whether your env-var override actually took effect.

```bash
# Pipe to less when your terminal is narrow
dh-waypoint config show | less -S

# Grep for a specific subsystem
dh-waypoint config show | grep -i csam
```

## `dh-waypoint process`

```bash
dh-waypoint process
```

One-shot batch processor: runs the classification + persistence pipeline once over enqueued items, then exits. Useful for catch-up runs after a long downtime, or for scripted batch jobs.

## `dh-waypoint reprocess`

```bash
dh-waypoint reprocess
```

Re-classifies already-indexed torrents through the current classifier. Idempotent. Run after editing CEL rules to apply the new logic to existing data without re-crawling.

The operation is staged through the queue, so it can be safely interrupted and resumed. Watch progress via Prometheus (`dh_waypoint_classifier_examined_total`).

## `dh-waypoint attribution`

Sub-commands powering the wantbridge and `*arr` grab-attribution flow. Not normally invoked by an operator — exposed for diagnostic use during incident response.

## `dh-waypoint migrate` (dev)

Wraps goose for dev/CI use. The worker auto-applies migrations on startup, so you don't need this in normal operation. Available for integration tests and local DB setup.

## `dh-waypoint gorm` (dev)

GORM tooling. Internal use; the surface is unstable.

## Running inside the container

In a deployed environment, run any CLI command via `docker exec`:

```bash
docker exec dh-waypoint dh-waypoint <subcommand>
```

For example, to dump the resolved config of a running container:

```bash
docker exec dh-waypoint dh-waypoint config show
```

To re-classify after editing rules in the container:

```bash
docker exec dh-waypoint dh-waypoint reprocess
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Application error (the worker, the resolver, or a sub-command failed) |
| `2` | CLI flag parsing error (urfave/cli convention) |

A subcommand that runs to completion always exits `0`. SIGTERM/SIGINT during `worker run` exits `0` after a clean shutdown.

## See also

- [Configuration](../configuration.md)
- [Examples / self-hoster quickstart](../../examples/README.md)
- [Upgrading](../../examples/README.md#upgrading)
- [Concepts / classification](../concepts/classification.md)
