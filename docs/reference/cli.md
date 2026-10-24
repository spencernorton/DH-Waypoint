# CLI reference

The DH-Waypoint binary is named `dh-waypoint`. Run with no arguments — or `--help` — for the live command list, sorted alphabetically:

```bash
dh-waypoint --help
```

`dh-waypoint <command> --help` shows each command's flags.

## Top-level commands

| Command | Purpose |
|---|---|
| `worker run` | Start one or more workers and block (what the bundled Compose files run) |
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
| `process` | One-shot: rematch the torrents given with `--infoHash` through the classification + persistence pipeline, then exit |
| `reprocess` | Re-classify already-indexed torrents through the current classifier |
| `anime-backfill` | Recompute the stored `is_anime` flag from torrent names |
| `backlog-local-classify` | Classify unmatched movie/TV rows locally, without queue jobs or API calls |
| `catalogue-policy-shadow` | Evaluate English and availability evidence without changing catalogue eligibility |
| `derived-backfill` | Fill derived `torrent_contents` columns for rows classified before they existed (alias `granularity-backfill`) |
| `episodes-backfill` | Re-parse 3+ digit or concatenated episode numbers in `tv_show` rows |
| `purge-content-types` | Count and sample out-of-scope content types; `--write` is refused in current builds (catalogue removal is disabled) |
| `refresh-alt-titles` | Backfill alternative titles onto existing TMDB content rows |
| `refresh-anime-titles` | Rebuild the anime alias table from AniDB and Anime-Lists data |
| `refresh-seeds` | Scrape public UDP trackers (BEP 15) for seeder and leecher counts |
| `release-fields-repair` | `freeze`, `apply` or `rollback` a bounded filename-field repair plan (see [Configuration](../configuration.md)) |
| `attribution recon` | Read-only report correlating Torznab results with Prowlarr grabs, qBittorrent state and `*arr` imports |

Commands with a `--write` flag only report until it is given; the `refresh-*` commands persist unless `--dryRun` is given. Pending migrations apply automatically on first database use; `migrate` and `gorm` exist only in the development binary (`go run ./internal/dev`), not in the release image.

## `dh-waypoint worker`

The entry point for running DH-Waypoint. The bundled Compose files run `worker run --all`; the image itself has no default command.

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
# Production — all workers (what the bundled Compose files run)
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
dh-waypoint classifier show --format json | jq '.workflows | keys'
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
| `path` | Dot-notation config key (e.g. `dht_crawler.scaling_factor`) |
| `Type` | Go type |
| `Value` | Currently resolved value |
| `Default` | Default if no override applies |
| `From` | Which resolver produced the value (`env`, `default`, or the config file path) |

The `From` column is the most useful single signal — it tells you exactly why a config has the value it does, including whether your env-var override actually took effect.

```bash
# Pipe to less when your terminal is narrow
dh-waypoint config show | less -S

# Grep for a specific subsystem
dh-waypoint config show | grep -i csam
```

## `dh-waypoint process`

```bash
dh-waypoint process --infoHash <hash> [--infoHash <hash> ...]
```

One-shot: rematches each given torrent through the classification + persistence pipeline, ignoring any existing match, then exits. Use `reprocess` for the whole catalogue.

## `dh-waypoint reprocess`

```bash
dh-waypoint reprocess
```

Re-classifies already-indexed torrents through the current classifier. Idempotent. Run after editing CEL rules to apply the new logic to existing data without re-crawling.

The operation is staged through the queue, so it can be safely interrupted and resumed.

## `dh-waypoint attribution`

`attribution recon` prints a one-shot, read-only report correlating Torznab results with Prowlarr grab history, qBittorrent state and `*arr` import outcomes. It runs only with `ATTRIBUTION_ENABLED=true` and falls back to the `WANTBRIDGE_*` `*arr` settings. Not normally invoked by an operator — exposed for diagnostic use during incident response.

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
| `0` | Success. An unknown command also exits `0` after logging `command not found`. |
| `1` | Startup failed (for example invalid configuration), or a command reported its own error (for example `torrent create`, `plugin validate`) |
| `2` | Any other command error, including flag errors; it is printed as a Go panic with a stack trace |

SIGTERM/SIGINT during `worker run` exits `0` after a clean shutdown.

## See also

- [Configuration](../configuration.md)
- [Examples / self-hoster quickstart](../../examples/README.md)
- [Upgrade and recovery](../../examples/README.md#upgrade-and-recovery)
- [Concepts / classification](../concepts/classification.md)
