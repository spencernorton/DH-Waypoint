# Armiger Ultra integration

[Armiger Ultra](https://github.com/spencernorton/armiger) labels torrents by
type from their names and file lists, on the CPU. DH-Waypoint uses it through
[`adapters/armiger`](../adapters/armiger/README.md), a small bridge that answers
the core's `classify` jobs by calling the Armiger container's
`POST /v1/classify`. Armiger's published image is used unchanged.

## Turn it on

1. In `.env`, add `armiger` to `COMPOSE_PROFILES`.
2. In [`plugins/armiger-ultra.plugin.yml`](../plugins/armiger-ultra.plugin.yml),
   set `enabled: true`.
3. Run `docker compose up -d`.

Compose then starts two services: `armiger-ultra`, the published
`ghcr.io/spencernorton/armiger` image pinned by `ARMIGER_VERSION` (default
`3.2.1`), and `armiger`, the bridge, built from this repository. The bridge
shares the `dh-waypoint` container's network namespace, listens on
`127.0.0.1:8095` and reaches Armiger over the Compose network.

Check it:

```sh
docker compose exec dh-waypoint dh-waypoint plugin list
docker compose exec dh-waypoint curl -s http://127.0.0.1:8095/v1/status
```

`"state":"ready"` means the bridge can reach Armiger; `"unavailable"` means
Armiger is still starting or has stopped.

## What it proposes

For each torrent the plugin's selector admits (by default, those nothing else
has labelled), the bridge sends the name, size and file list to Armiger and
reads its `category` head:

- A category DH-Waypoint has a content type for (movie, tv_show, music, ebook,
  audiobook, comic, game, software or xxx), at or above
  `ARMIGER_MIN_CONFIDENCE` (default `0.6`), becomes a `classify` proposal whose
  uncertainty is one minus Armiger's confidence.
- `course`, `unknown` and less confident answers abstain.
- If Armiger cannot be reached, the job is deferred and retried.

The core validates the proposal as it does for any processor, and no plugin
can delete a record. Armiger's other heads (adult, junk, anime, languages and
swarm viability) are not used.

## Limits

Armiger decides from metadata alone, and without a file list it reads only the
name. It has no category for photos: in our tests it called photo collections
`software` with about the same confidence it gave correct answers, so a higher
threshold does not filter them out. To review its proposals before they change
the catalogue, set `mode: shadow` in the plugin file.

## Changing the Armiger version

Set `ARMIGER_VERSION` in `.env` and run `docker compose up -d`. The bridge
reports the version in its identity, so DH-Waypoint stops sending it work until
the plugin is registered again: move the plugin file out of `plugins/` for 30
seconds, then back.
