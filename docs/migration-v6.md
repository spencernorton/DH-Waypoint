# Upgrading to DH-Waypoint 6.0

Version 6.0 is a major release pairing a leaner, faster Go backend (`-103K+`
lines of removed legacy/dead/orphaned code, zero-alloc lexer, gated debug SQL
logging, batched persistence locking) with the default-on **`dh-tmsd`** Rust
search and serving sidecar, non-root container execution (`UID 1000:1000`),
PostgreSQL 16 & 18 support, and a fail-closed pre-flight upgrade guard.

## Who needs to take action?

- **Fresh installations:** Zero manual steps. Run `dh-waypoint init && docker compose up -d`
  (or use `examples/docker-compose.pg18.yml` for a fresh PostgreSQL 18 stack).
- **Upgrading from `v5.12.0` – `v5.15.x` (PostgreSQL 16 or 18):** **No manual
  PostgreSQL 16 -> 18 dump/restore or manual SQL is required.** Keep your
  existing PostgreSQL 16 (or PostgreSQL 18) service and volume unchanged, create
  a verified backup, pass the instance-specific backup confirmation token once,
  and `v6.0.0` completes the upgrade automatically.
- **Upgrading from `v3.x`, `v4.x`, `v5.0`–`v5.11`, or stock Bitmagnet:** Direct
  jumps are intentionally refused (`exit 78`, zero database writes). Step
  through bridge release `v5.15.0` first, verify `dh-waypoint doctor --check-v6`,
  and then upgrade to `v6.0.0` (or start a fresh `v6.0.0` stack and stream
  NDJSON via `POST /import`).

## Hard-stop protection (`exit 78`)

Before Goose executes any schema change (serialized under a PostgreSQL advisory
lock), `v6.0.0` inspects the database strictly read-only:

1. **Empty database (no `goose_db_version` and no `torrents`):** Initializes
   schema epoch `6` immediately with zero flags.
2. **Existing `v6` database (`schema_major = 6` and `Goose >= 100`):** Starts
   normally with zero flags.
3. **Compatible `v5.12`–`v5.15` database (`Goose 81–83`, `schema_major` unset):**
   Stops cleanly with exit code `78` (`EX_CONFIG`) and performs **zero writes**
   until you confirm a backup using the exact 12-character DB fingerprint
   printed in the stop banner.
4. **Incompatible or inconsistent database (`Goose < 81`, `Goose 84–99`,
   `torrents` without `goose_db_version`, or mismatched `schema_major`):** Stops
   unconditionally with exit code `78` and performs **zero writes** even if a
   confirmation token is set.
5. **Accidental downgrade (`v5.15.0` pointed at a `v6` database):** Refuses to
   start with exit code `78`.

## Step-by-step upgrade from `v5.12` – `v5.15`

1. *(Recommended)* On `v5.15.0`, run a read-only readiness check:

   ```sh
   docker compose exec dh-waypoint dh-waypoint doctor --check-v6
   ```

   Record the printed `BACKUP` command and `UPGRADE_CONFIRM_V6_BACKUP=<fingerprint>` token.

2. Stop `dh-waypoint` and create a verified PostgreSQL backup:

   ```sh
   docker compose stop dh-waypoint
   docker compose exec -T postgres pg_dump -Fc -U bitmagnet -d bitmagnet > dh-waypoint-pre-v6-$(date +%F).dump
   ```

3. Update your `dh-waypoint` image tag to `ghcr.io/spencernorton/dh-waypoint:v6.0.0`
   (or `:v6`) and update container volume mount targets for non-root
   `UID 1000:1000` execution while keeping your existing `postgres` service and
   data volume unchanged:
   - Old: `/root/.config/bitmagnet` and `/root/.local/share/bitmagnet`
   - New: `/var/lib/dh-waypoint/config/bitmagnet` and `/var/lib/dh-waypoint/data/bitmagnet`
   - **Important:** Do **not** swap an existing PostgreSQL 16 volume onto
     `postgres:18-alpine` (`/var/lib/postgresql`) directly — PostgreSQL 18 uses
     a different data layout and would initialize an empty cluster. Keep your
     existing `postgres:16-alpine` (`/var/lib/postgresql/data`) service as-is;
     `v6.0.0` supports both PostgreSQL 16 and PostgreSQL 18 directly.
   - If your existing `dh-waypoint-config` / `dh-waypoint-data` volumes were
     created as `root:root` and `dh-waypoint` needs write access (e.g. rotated
     log files), align ownership once:

     ```sh
     docker run --rm -u 0 -v dh-waypoint-config:/cfg -v dh-waypoint-data:/data alpine chown -R 1000:1000 /cfg /data
     ```

4. Run the automatic one-time upgrade (either set
   `UPGRADE_CONFIRM_V6_BACKUP=<fingerprint>` in `.env` and run
   `docker compose up -d`, or invoke the CLI directly):

   ```sh
   docker compose run --rm dh-waypoint upgrade --confirm-backup=<fingerprint>
   docker compose up -d
   ```

5. Once started, `schema_major = 6` and `v6_upgrade_receipt` are recorded in
   `key_values`; you can remove `UPGRADE_CONFIRM_V6_BACKUP` from `.env`.
   The companion `dh-tmsd` service boots automatically on its fresh empty
   volume (failing closed back to PostgreSQL until the initial snapshot finishes)
   and `dhtms_sync` builds and maintains its search index automatically with
   zero manual steps. To run PostgreSQL-only, set `DH_TMS_READ_MODE=off`.

## Rollback

Downgrading a non-empty `v6` database back to `v5.x` via `goose down` is
intentionally blocked. To revert to `v5.15.0`, restore the pre-`v6` PostgreSQL
backup taken in Step 2 into a clean volume and start `v5.15.0`.
