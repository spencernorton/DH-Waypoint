# Security model and hardening

DH-Waypoint offers optional API keys and independently authenticated processor,
private-torrent and evidence endpoints. When the general HTTP key is unset,
GraphQL (including mutations), REST, import and metrics trust their network.
Use the built-in credentials and network/proxy controls described below.

This page is the operator's reference for how to deploy DH-Waypoint without making yourself the next obvious target.

## Threat model

**In scope.** A self-hoster's deployment exposed via a single host with port `3333` reachable. The operator's IP visible on the public BitTorrent DHT. An open `/torznab/api` endpoint. Misconfigured Postgres password. Loss/leak of `TORZNAB_API_KEY` or `EVIDENCE_WEBHOOK_SECRET`.

**Out of scope.** Zero-day vulnerabilities in Postgres or the Go runtime. Supply-chain compromise of a base image. Kernel-level exploits or container escape. Compromise of upstream `*arr` services (DH-Waypoint trusts whatever data they send).

The single load-bearing assumption is that the host running DH-Waypoint is trusted. There is no internal mTLS between DH-Waypoint and Postgres in the bundled stack.

## Network attack surface

Both Compose files publish HTTP port `3333` on every host interface (`${DH_WAYPOINT_HTTP_PORT:-3333}:3333`). Bind it to `127.0.0.1` or firewall it unless other hosts must reach it, and review every API below. All of these paths answer on it:

| Path | Auth (default) | Sensitive? |
|---|---|---|
| `/graphql` | none — gates on `HTTP_SERVER_API_KEY` if set | Yes — full mutation surface |
| `/torznab/api` | none — gates on `TORZNAB_API_KEY` if set | Yes if `*arr` uses it |
| `/metrics` | none — gates on `HTTP_SERVER_API_KEY` if set | Information disclosure (DB sizes, infohash counts) |
| `/import` | none — gates on `HTTP_SERVER_API_KEY` if set | Yes — accepts NDJSON bulk-import |
| `/api/v1/*`, `/api/v2/*` | none — gates on `HTTP_SERVER_API_KEY` if set | Catalogue reads |
| `/evidence/arr/<instance>` | none — gates on `X-Evidence-Token` matching `EVIDENCE_WEBHOOK_SECRET` if set | Yes — writes evidence that outranks the classifier |
| `/api/processors/v1/*` | `Authorization: Bearer` matching `PROCESSORS_ADMIN_TOKEN`; 503 when unset | Yes — registers processors |
| `/announce/<passkey>`, `/scrape/<passkey>`, `/api/private/v1/*` | tracker passkey or `TRACKER_ADMIN_TOKEN`; off unless `TRACKER_ENABLED` | Yes if the tracker is on |
| `/status` | none, by design | Liveness only |

**Do not expose port `3333` directly to the internet.** Always front it with a reverse proxy that adds auth.

## Authentication tiers

Three deployment shapes, in increasing order of exposure tolerance.

### Tier 1 — open (default)

Use only on a tailnet, ZeroTier, LAN, or container-network-only deployment. DH-Waypoint accepts all requests. Network isolation is the entire control.

### Tier 2 — API key

- `HTTP_SERVER_API_KEY` — gates `/graphql`, `/import`, `/metrics` and `/api/v1/*` (`Authorization: Bearer` or `X-API-Key`, constant-time compare). Surfaces with their own credentials (Torznab, tracker, processors, private API, evidence) are not double-gated; `/status` stays public.
- `TORZNAB_API_KEY` — gates `/torznab/api` (constant-time compare; timing-safe). Required when the `*arr` clients are on a different host or network than DH-Waypoint.
- `EVIDENCE_WEBHOOK_SECRET` — gates `/evidence/arr/*` (compared against the `X-Evidence-Token` header on each webhook).

The general HTTP key grants the full GraphQL mutation surface. It is an
operator credential; do not distribute it as a read-only or processor-specific
credential. Processor administration has its own token. No per-plugin
authorization scopes are implied by a plugin registration.

GraphQL accepts only JSON POST bodies. Its byte, parser, selection, paging and
input-list bounds are documented in the [GraphQL reference](../reference/graphql-api.md).
Catalogue requests have a 30-second context deadline and bounded socket I/O;
the server also bounds header reads to 10 seconds and idle keep-alive connections
to 60 seconds. These controls complement authentication and proxy concurrency
limits. They do not turn a trusted-network installation into a public service.

Generate keys with at least 256 bits of entropy:

```bash
openssl rand -hex 32
```

Both keys are read at process startup. Rotation requires a process restart; see [Key rotation](#key-rotation) below.

### Tier 3 — reverse-proxy auth

For public-internet exposure. Use Caddy, Traefik, NPM, Authelia, or Cloudflare Access in front. The proxy does the auth, DH-Waypoint trusts whatever traffic the proxy forwards.

Defense-in-depth: keep `TORZNAB_API_KEY` and `EVIDENCE_WEBHOOK_SECRET` set even when the proxy is doing the heavy lifting. A misconfigured proxy that silently lets traffic through still hits a 401 from DH-Waypoint if the keys aren't there.

## DHT and IP exposure

DH-Waypoint participates in the public BitTorrent DHT. There is no setting that disables this; it is what DH-Waypoint fundamentally *does*. Consequence:

- Your IP is published to the global DHT routing table.
- During BEP-9 metainfo fetch, your IP appears in swarm peer lists.
- Peer-tracking services (e.g. "I know what you download") observe and record this.

DH-Waypoint does *not* download torrent payloads — only metadata. The exposure window is bounded to the metadata phase. Still, for any deployment where IP exposure is a concern, the standard mitigation is:

- Run DH-Waypoint behind a suitable network tunnel if your deployment requires it.
- Use a VPN provider that supports port forwarding so DHT inbound traffic gets symmetric reach.

CSAM — the one category where post-fetch detection is too late — has its own pre-fetch double-hash defense. See [csam-defense.md](../csam-defense.md).

## CSAM defense

Briefly: a pre-fetch double-hashed bloom filter rejects known-CSAM infohashes *before* DH-Waypoint ever opens a TCP connection to a peer. Defaults are safe (enabled, no feeds = NoOp). Layered architecture in [csam-defense.md](../csam-defense.md).

The post-classify regex (`keywords.banned`, `PROCESSING_MODE=full` only) provides a second layer for any infohash that slips past the pre-fetch filter. While catalogue removal is disabled its delete is held rather than applied, but the self-export still records the observation. The self-export pipeline contributes back to community feeds, so each operator's observations harden the pre-fetch defense for the next operator.

## Database hardening

`POSTGRES_PASSWORD` is the only thing standing between an attacker on the host and your indexed corpus.

- Generate it with `openssl rand -hex 32`. Avoid passwords with shell-special characters.
- Never commit `.env` files to git. Add them to `.gitignore` from day one.
- Rotate immediately on any suspected leak.
- The default Postgres user is `bitmagnet` with full database privileges on the `bitmagnet` database. Bind to localhost or the docker compose internal network — do not expose `5432` externally.

For the bundled compose stack, Postgres is on the internal docker network only and is not reachable from the host's external interfaces.

## Reverse-proxy recipe

Caddy template for a public deployment with mixed auth:

```caddyfile
dh-waypoint.example.com {
  # /torznab/api: gated on TORZNAB_API_KEY only — *arr clients hit it
  reverse_proxy /torznab/* dh-waypoint:3333

  # /evidence/arr/*: gated on EVIDENCE_WEBHOOK_SECRET only
  reverse_proxy /evidence/* dh-waypoint:3333

  # Everything else: forward_auth or basic_auth
  @internal {
    not path /torznab/* /evidence/*
  }
  basic_auth @internal {
    admin <bcrypt_hash>
  }
  reverse_proxy @internal dh-waypoint:3333
}
```

Generate the bcrypt hash with `caddy hash-password`. Use a 12+ character random password.

## Webhook auth (`X-Evidence-Token`)

Every `*arr` Connect → Webhook entry needs the `X-Evidence-Token` Custom Header set to the same value as `EVIDENCE_WEBHOOK_SECRET`. The webhook URL pattern is:

```
http://<dh-waypoint-host>:3333/evidence/arr/<instance-label>
```

`<instance-label>` is operator-chosen — it ends up in the `label_evidence.source_instance` column for traceability (e.g. `sonarr-main`, `sonarr-anime`, `radarr-4k`).

Mismatched or missing token → request rejected, no row written, increments `dh_waypoint_evidence_events_rejected_total`.

## Key rotation

### `TORZNAB_API_KEY`

1. Generate the new key: `openssl rand -hex 32`.
2. Update `TORZNAB_API_KEY` in the DH-Waypoint env (`.env` file or secret manager), restart the DH-Waypoint container.
3. Update each `*arr` indexer's API Key field to the new value.
4. Test each indexer connection.
5. The old key is now dead — no need to retire it explicitly.

### `EVIDENCE_WEBHOOK_SECRET`

Mirroring matters here — webhook signal is dropped during the gap.

1. Generate the new value.
2. Update each `*arr` Connect → Webhook entry's `X-Evidence-Token` to the new value first.
3. Update `EVIDENCE_WEBHOOK_SECRET` in DH-Waypoint's env and restart.
4. The webhook ingestor will reject any in-flight `*arr` retries that still carry the old token — `*arr` will retry with the new token, and the data eventually lands.

For zero gap, deploy a forward-compatible DH-Waypoint build that accepts both old and new tokens for the rotation window — not currently shipped; ask in Discussions if you need it.

## Logging and audit

`LOG_LEVEL=info` is the production default and captures auth failures, webhook rejection, and CSAM-defense activity. `LOG_LEVEL=debug` captures every Torznab query — useful for incident response, very loud.

All logs go to stdout. No telemetry, no upstream phone-home — except `CSAM_BLOCKLIST_EXPORT_UPSTREAM_URL` if you opt in.

## Container hardening

The bundled Dockerfile already does most of what you'd want:

- Retains root for compatibility with the optional `/root/.config/bitmagnet` and
  `/root/.local/share/bitmagnet` state mounts. An operator can use a non-root
  UID after arranging writable state paths; this is a separate migration.
- The image is small (Alpine base, multi-stage build).
- No build tooling in the runtime layer.

If you want to harden further:

- `read_only: true` in compose. DH-Waypoint doesn't need a writable rootfs at runtime.
- `cap_drop: [ALL]`. The only capability that might be needed is `CAP_NET_BIND_SERVICE`, and only if you've set `DH_WAYPOINT_PEER_PORT` below `1024`. Keep it ≥ `1024` and you can drop everything.
- `tmpfs` on `/tmp` if you need any writes at all.
- Mount Postgres data on a dedicated volume; never bind-mount the database directory from the host filesystem (file-permission drift).

## Vulnerability reporting

Report security issues via the repo's [SECURITY.md](../../SECURITY.md) — don't open a public issue for unpatched vulnerabilities. The maintainer triages on a best-effort basis; there is no formal SLA or bounty.

## See also

- [Configuration](../configuration.md)
- [Deployment](../../examples/README.md)
- [CSAM defense](../csam-defense.md)
