# Plugins

DH-Waypoint reads every `*.plugin.yml` in this folder every 30 seconds. Each
template here starts with `enabled: false`; set it to `true` to turn it on.

| Template | What it does | Needs |
|---|---|---|
| [`regex-classifier.plugin.yml`](regex-classifier.plugin.yml) | Labels and tags torrents from patterns in their name or file paths | Nothing |
| [`armiger-ultra.plugin.yml`](armiger-ultra.plugin.yml) | Labels what is left with the Armiger Ultra model, on the CPU | `armiger` in `COMPOSE_PROFILES` |
| [`openrouter.plugin.yml`](openrouter.plugin.yml) | Labels what is still left with a hosted model through OpenRouter | `OPENROUTER_API_KEY`, `openrouter` in `COMPOSE_PROFILES` |

They run in that order. A torrent one of them labels is skipped by the next,
so you pay OpenRouter only for what the other two could not label. Each
template's header has its three set-up steps.

Check what is registered with
`docker compose exec dh-waypoint dh-waypoint plugin list`. If you change the
model or Armiger version behind a file-managed plugin, DH-Waypoint detects the
new service identity on the next 30-second sync tick and re-registers it
automatically.

Writing your own: [Plugin & Adapter SDK](../docs/plugins-sdk.md), [plugin files](../docs/processors/plugin-files.md), and the
[processor protocol](../docs/processors/v1.md).

## Storage & curation plugin: LEAN MODE

For hosts with limited RAM or disk space, [`lean-mode/`](lean-mode/) ships three declarative presets for the built-in **LEAN MODE** plugin (`1080p-lean.yml`, `4k-remux-best.yml`, and `small-footprint-x265.yml`). Each keeps only the single best release per movie or TV episode (and optionally per TV season pack) using a three-tier YAML rule engine (`required`, `preferred`, `if_found`).

All bundled presets ship disabled (`enabled: false`, `mode: observe`), and the plugin itself defaults to off (`LEAN_MODE_ENABLED=false`):

1. Pick or edit a preset in `plugins/lean-mode/` and set `enabled: true` (keep `mode: observe` to preview).
2. Set `LEAN_MODE_ENABLED=true` and `LEAN_MODE_CONFIG_FILE=/plugins/lean-mode/<preset>.yml` in `.env`, then restart `dh-waypoint`.
3. Verify winners with `dh-waypoint lean status` or `dh-waypoint lean diff`, then opt into `mode: serve` or double-locked `mode: prune`.

Because LEAN MODE policies live in `plugins/lean-mode/` rather than `plugins/*.plugin.yml`, the 30-second processor plugin loader ignores them cleanly. See [docs/lean-mode.md](../docs/lean-mode.md).
