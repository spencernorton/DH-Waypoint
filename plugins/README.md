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
model or Armiger version behind a plugin, move its file out of this folder for
30 seconds and back in, so DH-Waypoint registers the new version.

Writing your own: [plugin files](../docs/processors/plugin-files.md) and the
[processor protocol](../docs/processors/v1.md).
