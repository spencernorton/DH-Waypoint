# Plugin files: drag-in processor registration

A plugin file is a small YAML document that declares one processor
registration. Drop it into the plugins directory and DH-Waypoint does
the rest — no rebuild, no restart, no API calls.

```sh
cp examples/plugins/01-filter-list.plugin.yml plugins/
# registered within 30 seconds; verify:
dh-waypoint plugin list
```

## Where files live

The processors worker reconciles one directory every 30 seconds:

- config: `processors.plugins_dir` (env `PROCESSORS_PLUGINS_DIR`)
- default: `plugins` (relative to the process working directory;
  the compose stacks mount `./plugins` to `/plugins`)
- empty string disables directory sync entirely
- a missing directory is fine: sync is a no-op

File lifecycle:

| Action | Effect |
|---|---|
| Add `<id>.plugin.yml` | Plugin registered |
| Edit the file | Registration updated under a new generation |
| Delete the file, or set `enabled: false` | Plugin deregistered |
| Introduce a typo while editing | File skipped and logged; the previous registration stays |
| File ID collides with an API-created registration | File skipped and logged; the API registration is never modified |

Ownership is persisted on the registration itself (`source: file`),
so a file deleted while the core is stopped is still deregistered
by the first sync after restart. Re-registering through the
[operator API](operator-api.md) claims the registration for the API
(`source: api`); file sync then leaves it alone.

## File format

```yaml
id: my-filter              # optional; defaults to the file name
enabled: true              # false keeps the file but deregisters
driver: builtin/filter-list
endpoint: ""               # http processor services only
mode: shadow               # shadow (record only) | apply
state: active              # active | paused | draining
priority: 100              # lower runs earlier within a stage
auth_env: ""               # DH_WAYPOINT_PROCESSOR_TOKEN_* name, never a value
selector:                  # optional; see below
  content_types: [movie]
configuration:             # driver-specific, becomes the job JSON
  reason: example
```

Rules:

- Unknown keys are an error (`dh-waypoint plugin validate <file>`
  catches them before deployment). Nothing is silently ignored.
- `id` must match the ID the service reports in its manifest
  (for `http` drivers) — registration fails otherwise.
- `configuration` must not contain credentials; the core rejects
  credential-looking keys. Secrets belong in `auth_env`-named
  variables or the credential directory.
- Files are capped at 64 KiB, at most 64 files are read, and the
  registry itself caps at 16 processors.

## Drivers

### `builtin/filter-list` — filter documents

Excludes (or tags) releases by name/file evidence. Exclusions are
reversible policy proposals; plugins can never delete torrents.
Template: `examples/plugins/01-filter-list.plugin.yml`.

| Key | Meaning |
|---|---|
| `block_patterns` / `allow_patterns` | Name regexes; allow wins |
| `blocked_extensions`, `blocked_release_groups` | Exact-match lists |
| `min_size_bytes`, `max_size_bytes` | Size bounds (0 = none) |
| `reason` | Recorded with every proposal |
| `tag_only`, `tags` | Tag instead of excluding |

### `builtin/regex-weights` — category documents

Static weighted rules stamp content type, video fields, tags and scores.
They do not learn or read retained learned state; use an external HTTP
processor for calibration. Built-ins support feedback logging only.
This is the "custom dataset" surface: rules can be generated from
your own lists. Template: `examples/plugins/02-categories-regex.plugin.yml`.

Each rule: `id`, `pattern`, `target` (`name`|`files`|`any`),
`weight`, optional `dimension`, `content_type`, `video_resolution`,
`video_source`, `video_codec`, `release_group`, `add_tags`.
Top level: optional `exclude_dimension` + `exclude_threshold` +
`exclude_reason` turn accumulated weight into an exclusion proposal.

### `builtin/tracker-liveness`

Acts on tracker-observed swarm facts (`min_seeders`,
`exclude_dead`, live/dead tags and weights). A swarm the core has
never scraped is *unknown*, never dead.
Template: `examples/plugins/03-tracker-liveness.plugin.yml`.

### Independent LLM adapter

`builtin/llm` is retired. Run the [standalone LLM adapter](llm.md) and
register it as `http`. Provider URLs, prompts, model settings, credentials
and inference budgets belong in that service's configuration, never in
this file. The `04` and `05` examples show the new registrations.

### `http` — your own service, any language

Implements `GET /v1/manifest`, `GET /v1/status`,
`POST /v1/process` ([protocol](v1.md)). Start from
`examples/processors/reference` (Go, 79 lines) or
`examples/processors/independent` (Python stdlib, 89 lines), probe
with `dh-waypoint plugin test <endpoint>`, then register with
`06-http-service.plugin.yml`.

## Selectors: run plugins only where they pay

A selector is evaluated per torrent immediately before dispatch;
non-matching torrents record an abstention without a network call:

| Key | Meaning |
|---|---|
| `content_types` / `exclude_types` | Core-stamped type filter |
| `extensions` | Primary file extension filter |
| `is_anime` | Anime backbone verdict |
| `has_content_match` | Whether a title identity is attached |
| `excluded` | Current exclusion state |
| `min_uncertainty` | Only torrents earlier stages were unsure about |
| `required_tags` | All listed tags present |
| `score_thresholds` | Accumulated score bounds per dimension |
| `sample_basis_points` | Deterministic sample (10000 = all) — the cost knob for LLM plugins |

## Rollout pattern

1. Copy a template, keep `mode: shadow`.
2. Watch proposals: processor status API, `dh-waypoint plugin list`,
   `dh_waypoint_processors_outcomes_total` metrics.
3. When shadow results are right, set `mode: apply`.
4. Stack deliberately: lower `priority` runs first within a stage;
   stages run `classify → extract → match → language → policy → embed`.

## Processing modes compose with plugins

`PROCESSING_MODE` decides how much the *core* does before plugins
see a torrent: `full` (explicit built-in classifier), `basic` (default; coarse
category from file extensions only), `raw` (nothing). Most
plugin-first installs want `basic` or `raw` so their filter and
category documents — not core defaults — decide outcomes. See
[configuration](../configuration.md).
