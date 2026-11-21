# Upgrading to DH-Waypoint 6.0

Version 6.0 is a major release built around a leaner backend (`-100K+` lines of
removed legacy/zombie code), non-root container execution (`UID 1000:1000`),
PostgreSQL 18 default packaging, and a fail-closed pre-flight upgrade guard.

## Who needs to take action?

- **Fresh installations:** Zero manual steps. Run `dh-waypoint init && docker compose up -d`.
- **Upgrading from `v5.12.0` – `v5.15.x` (PostgreSQL 16 or 18):** **No manual
  PostgreSQL 16 → 18 dump/restore or manual SQL is required.** Create a backup,
  pass the instance-specific backup confirmation token once, and `v6.0.0`
  completes the upgrade automatically.
- **Upgrading from `v3.x`, `v4.x`, `v5.0`–`v5.11`, or stock Bitmagnet:** Direct
  jumps are intentionally refused (`exit 78`, zero database writes). Step
  through bridge release `v5.15.0` first, verify `dh-waypoint doctor --check-v6`,
  and then upgrade to `v6.0.0` (or start a fresh `v6.0.0` stack and stream
  NDJSON via `dh-waypoint import`).

## Hard-stop protection (`exit 78`)

Before Goose executes any schema change, `v6.0.0` inspects the database
read-only:

1. **Empty database:** Initializes schema epoch `6` immediately with zero flags.
2. **Existing `v6` database (`schema_major = 6`):** Starts normally with zero flags.
3. **Compatible `v5.12`–`v5.15` database (`Goose 81–83`):** Stops cleanly with
   exit code `78` (`EX_CONFIG`) and performs **zero writes** until you confirm a
   backup using the exact 12-character DB fingerprint printed in the stop banner.
4. **Incompatible older database (`Goose < 81`) or active retired `builtin/llm`
   / `bitagent.processor.v1` rows:** Stops unconditionally with exit code `78`
   and performs **zero writes** even if a confirmation token is set.
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

3. Update your image tag to `ghcr.io/spencernorton/dh-waypoint:v6.0.0` (or
   `:v6`) and update container volume targets for non-root `UID 1000:1000`
   execution:
   - Old: `/root/.config/bitmagnet` and `/root/.local/share/bitmagnet`
   - New: `/var/lib/dh-waypoint/config` and `/var/lib/dh-waypoint/data`
   - *(Optional)* Keep your existing `postgres:16-alpine` service unchanged —
     `v6.0.0` supports both PostgreSQL 16 and PostgreSQL 18 directly.

4. Run the automatic one-time upgrade (either set
   `UPGRADE_CONFIRM_V6_BACKUP=<fingerprint>` in `.env` and run
   `docker compose up -d`, or invoke the CLI directly):

   ```sh
   docker compose run --rm dh-waypoint upgrade --confirm-backup=<fingerprint>
   docker compose up -d
   ```

5. Once started, `schema_major = 6` and `v6_upgrade_receipt` are recorded in
   `key_values`; you can remove `UPGRADE_CONFIRM_V6_BACKUP` from `.env`.

## Rollback

Downgrading a non-empty `v6` database back to `v5.x` via `goose down` is
intentionally blocked. To revert to `v5.15.0`, restore the pre-`v6` PostgreSQL
backup taken in Step 2 into a clean volume and start `v5.15.0`.
