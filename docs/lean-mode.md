# LEAN MODE Catalog Curation Plugin

`LEAN MODE` is a 100% opt-in (`enabled: false` by default) catalog curation extension for DH-Waypoint that retains **only the single best release per movie or TV episode** according to a declarative YAML profile (plus optional warm runner-ups for swarm-death and payload-proof failover).

## Why Hybrid Three-Phase Curation

LEAN MODE implements a progressive three-phase lifecycle so operators can verify winner accuracy before narrowing serving or reclaiming disk:

- **Phase 1 — `observe` (default when enabled):** Evaluates every linked movie and TV slot against the three-tier rule engine and persists ranked winners in `lean_winners`. Modifies zero rows in `torrents` or `torrent_contents`, changes zero consumer API responses, and triggers zero `processor_publication_sources` revision bumps.
- **Phase 2 — `serve` (non-destructive canonical projection):** Narrows consumer APIs (Torznab via `profile.lean_profile`, REST via `?lean_profile=<id>`, and optionally global `LEAN_MODE_SERVE_GLOBALLY=true` across Torznab/REST/GraphQL) to rank-1 winners while leaving all underlying torrents intact. Multiple named profiles (for example `1080p-lean` and `4k-remux-best`) can coexist on the same database simultaneously.
- **Phase 3 — `prune` (opt-in active pruning + config-digest tombstones):** Requires both `mode: prune` and explicit `prune.confirm_destructive: true`. Physically deletes dominated and disqualified losers in bounded `FOR UPDATE SKIP LOCKED` batches, protects all private/operator/evidence/quarantined rows, and writes compact `lean_tombstones` keyed by `(info_hash)` and bound to the profile's SHA-256 `config_digest`. Both the DHT crawler triage stage and `/import` skip active tombstones in one indexed lookup, eliminating re-crawl churn.

## Three-Tier Rule Engine

Every candidate release in a movie or TV slot passes through three deterministic tiers:

1. **`required` (Hard Gate):** Disqualifies any release missing mandatory constraints, including resolution floor/ceiling (`min_resolution`, `max_resolution`, `resolutions_allowed`), `exclude_cam_ts`, `exclude_3d`, allowed/blocked codecs (family-aware `H264`/`x264` and `HEVC`/`x265`), sources, modifiers, languages (`languages_required`, `require_english_audio`), HDR/audio requirements, `min_seeders`, per-slot-kind size bounds (`max_size_mb_movie`, `max_size_mb_episode`, `max_size_mb_season_pack`), release group allow/blocklists, `require_files_info`, `require_armiger_approved`, `require_payload_proof`, and bounded RE2 `exclude_title_patterns`.
2. **`preferred` (Ordered Ranking):** Ranks surviving candidates deterministically via either `weighted` scoring (with an explicit `equivalence_band`) or strict `lexicographic` tuple ordering across resolution, source (`REMUX > BluRay > WEBDL > WEBRip > TV`), codec, HDR (`DOLBY_VISION > HDR10_PLUS > HDR10 > HDR > SDR`), audio (`ATMOS > TRUEHD > DTS_X > DTS > EAC3`), preferred release groups, and size efficiency (`target_window`, `prefer_smaller`, `prefer_larger`).
3. **`if_found` (Crowd Refinement / Tie-Breaker):** Refines winners when multiple releases pass `required` and land inside the top `preferred` equivalence band, using an ordered list from `repack_proper`, `preferred_group`, `highest_seeders`, `clean_single_media_file`, `embedded_subtitles`, `newest_verified`, `smallest_size_in_band`, and `largest_size_in_band`. Ties terminate on `info_hash ASC`, guaranteeing bit-for-bit determinism independent of input order.

## Edge-Case Guarantees

- **TV Episode Slots, Multi-Episode Files, and Season Packs:** Configured via `scope.season_pack_policy`:
  - `separate_slot` (default): Keeps one best release per episode (`S01E01`), one best per multi-episode file (`S01E01+E02`), one best per season pack (`S01`), and one best per series pack (`SERIES`). Ideal for Sonarr/Prowlarr workflows.
  - `prefer_pack`: When a viable season pack wins `S01`, individual `S01E*` episode releases are subsumed while uncovered airing seasons retain their per-episode winners.
  - `prefer_individual`: Retains individual episode winners and subsumes redundant season packs for seasons with episode coverage.
- **Unlinked and Non-Video Releases:** `scope.unlinked_policy` (`pass_through` or `exclude_from_serve`) and `scope.non_video_policy` (`pass_through` or `exclude_from_serve`) keep non-video categories (`music`, `ebook`, `audiobook`, `software`, `game`) and unlinked rows untouched by default.
- **Swarm-Death, Proof-Expiry, and Hysteresis Resilience:**
  - `resilience.keep_runner_up` retains warm standby releases (`rank = 2..K`) protected from pruning so sudden swarm death or payload-proof expiry promotes the next best candidate without waiting for DHT rediscovery.
  - `resilience.hysteresis` prevents marginal rank flapping (`min_preferred_delta`, `min_seeder_ratio`, `incumbent_grace_period`) while allowing `REPACK`/`PROPER` releases in the same tier to dethrone immediately (`allow_repack_override: true`).
  - When a slot's incumbent and all runner-ups die, `prune.revive_slot_on_winner_death: true` clears that slot's tombstones so the crawler re-admits fresh swarms.
  - Changing any rule in the YAML file changes `config_digest`, immediately rendering prior tombstones inactive.
- **Safety and Revision Invariance:**
  - Serving filters always compose inside `serving.Policy.TorrentCondition`; LEAN MODE can only narrow consumer results, never bypass publication review, payload proof, private-torrent, adult, or blocklist gates.
  - `lean_winners` (`ON DELETE CASCADE` on `torrents`) and `lean_tombstones` store zero torrent names or file paths, are classified in the CSAM removal registry, and never bump `processor_publication_sources.revision` on retained winners.
  - Pruning enforces a hard protected-row fence (never deleting private torrents, canonical labels, `*arr`/qBittorrent `label_evidence`, `torrent_grab_attempts`, manual hints, active holds, quarantined entries, or privacy-tagged torrents) plus a `max_delete_fraction_per_cycle` circuit breaker.

## Bundled Presets

Three ready-to-customize declarative presets ship disabled by default under `plugins/lean-mode/`:

- `plugins/lean-mode/1080p-lean.yml` — Balanced 1080p-first catalog with 1 warm runner-up and separate season-pack slots.
- `plugins/lean-mode/4k-remux-best.yml` — Reference 2160p REMUX/BluRay + Dolby Vision/HDR10+ + Atmos/TrueHD profile.
- `plugins/lean-mode/small-footprint-x265.yml` — Storage-minimized HEVC/x265/AV1 profile with strict per-slot size caps and `season_pack_policy: prefer_pack`.

## CLI Workflow

```sh
# 1. Validate YAML syntax, schema constraints, and canonical digest
dh-waypoint lean validate plugins/lean-mode/1080p-lean.yml

# 2. Run non-destructive observe pass and inspect JSON summary
dh-waypoint lean reconcile --config plugins/lean-mode/1080p-lean.yml --mode observe

# 3. Inspect exact 3-tier decision breakdown for a specific movie or TV show
dh-waypoint lean explain --config plugins/lean-mode/1080p-lean.yml --type movie --source tmdb --id 550

# 4. Check persisted profile state, runner-ups, and active tombstones
dh-waypoint lean status --profile 1080p-lean

# 5. Clear tombstones for a profile or slot manually if needed
dh-waypoint lean clear-tombstones --profile 1080p-lean --slot movie:tmdb:550
```

## Runtime Environment Variables

- `LEAN_MODE_ENABLED=false` (default `false`; set `true` alongside `LEAN_MODE_CONFIG_FILE` to activate the background reconciler).
- `LEAN_MODE_CONFIG_FILE=/path/to/profile.yml`
- `LEAN_MODE_INTERVAL=15m`
- `LEAN_MODE_TITLE_BATCH_SIZE=200`
- `LEAN_MODE_SERVE_GLOBALLY=false`
