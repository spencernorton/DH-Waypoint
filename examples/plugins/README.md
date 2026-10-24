# Plugin file templates

Copy any `*.plugin.yml` file here into your plugins directory
(`plugins/` next to your compose file, or `PROCESSORS_PLUGINS_DIR`)
and DH-Waypoint registers it within 30 seconds — no rebuild, no
restart. Edit the file to update the plugin; delete it (or set
`enabled: false`) to remove it.

Every template starts in `mode: shadow`: the plugin runs and its
proposals are recorded, but the catalogue does not change. Watch
the results (`dh-waypoint plugin list`, the processors status API, or
`dh_waypoint_processors_*` metrics), then flip to `mode: apply`.

| File | Driver | What it does |
|---|---|---|
| `01-filter-list.plugin.yml` | `builtin/filter-list` | Exclude releases by name pattern, extension, group, or size |
| `02-categories-regex.plugin.yml` | `builtin/regex-weights` | Stamp content type / resolution / tags from weighted name rules |
| `03-tracker-liveness.plugin.yml` | `builtin/tracker-liveness` | Tag or exclude swarms by tracker-observed seeders |
| `04-llm-local-ollama.plugin.yml` | `http` | Independent adapter for local OpenAI-compatible inference |
| `05-llm-hosted.plugin.yml` | `http` | Independent adapter for hosted OpenAI-compatible inference |
| `06-http-service.plugin.yml` | `http` | Call your own processor service in any language |
| `07-armiger-ultra.plugin.yml` | `http` | Connect [Armiger / Armiger Ultra](../../docs/armiger-integration.md) neural classification, swarm viability, and safety gates |

Guides: [plugin files](../../docs/processors/plugin-files.md) ·
[connecting an LLM](../../docs/processors/llm.md) ·
[API guide](../../docs/api-guide.md)

Validate before deploying: `dh-waypoint plugin validate <file>`.
Probe an HTTP service: `dh-waypoint plugin test <endpoint>`.
