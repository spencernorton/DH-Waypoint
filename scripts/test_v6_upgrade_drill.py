#!/usr/bin/env python3
"""End-to-end Docker & Compose verification of the v5.15 -> v6.0 hard-stop & auto-upgrade contract.

Standalone drill (`drill`):
  1. Legacy v5.15.0 initializes schema 83, imports public + private fixtures, emits doctor fingerprint.
  2. Candidate v6.0.0 WITHOUT UPGRADE_CONFIRM_V6_BACKUP exits 78 with ZERO DB mutations.
  3. Candidate v6.0.0 WITH wrong fingerprint exits 78 with ZERO DB mutations.
  4. Non-superuser PostgreSQL role succeeds on `v6 upgrade --check` (fallback fingerprint).
  5. Candidate v6.0.0 CLI `upgrade --confirm-backup=<fp>` completes automatic upgrade
     (single version-100 row, schema_major='6', v6_upgrade_receipt) and serves exact public/private results.
  6. Candidate v6.0.0 restarts cleanly WITHOUT UPGRADE_CONFIRM_V6_BACKUP.
  7. Legacy v5.15.0 pointed at upgraded v6 DB refuses to start (exit 78, symmetric guard).
  8. Simulated pre-v5.12 DB (Goose 70) hits unconditional exit 78 even with Goose 70's OWN fingerprint.

Compose drill (`compose_drill`):
  1. Starts a v5.15.0 Compose stack (PG 16 at /var/lib/postgresql/data, root-owned /root/.config/bitmagnet
     volume containing config.yml with http_server.api_key) and imports public + private fixtures.
  2. Swaps in the repository's v6 docker-compose.yml reusing the EXACT same named volumes.
  3. Proves exit 78 hard stop fires with unchanged DB row digest (PG volume NOT silently replaced).
  4. Passes UPGRADE_CONFIRM_V6_BACKUP=<fp>, verifies automatic upgrade to schema_major='6' with
     exactly one version-100 migration row, AND proves the v5 root-owned config.yml API key is
     still loaded and enforced under non-root UID 1000:1000 (401 unauth, 200 keyed, 0 private leaks).
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
PUBLIC_HASH = "a1" * 20
PRIVATE_HASH = "b2" * 20
NAME = "Waypoint.Upgrade.Drill.2026.1080p.REMUX"


def run(*args, check=True, timeout=120, input_text=None, cwd=None, env=None):
    res = subprocess.run(
        [str(a) for a in args],
        text=True,
        capture_output=True,
        timeout=timeout,
        input=input_text,
        cwd=cwd,
        env=env,
    )
    if check and res.returncode != 0:
        raise RuntimeError(
            f"command failed ({res.returncode}): {' '.join(map(str, args))}\n"
            f"STDOUT:\n{res.stdout}\nSTDERR:\n{res.stderr}"
        )
    return res


def http_req(base, path, payload=None, headers=None):
    data = None if payload is None else payload.encode()
    hdrs = {"Content-Type": "application/json"}
    if headers:
        hdrs.update(headers)
    req = urllib.request.Request(base + path, data, hdrs)
    with urllib.request.urlopen(req, timeout=10) as resp:
        return resp.status, resp.read()


def http_status(base, path, headers=None):
    try:
        status, _ = http_req(base, path, headers=headers)
        return status
    except urllib.error.HTTPError as e:
        return e.code


def wait_ready(base, container_name, timeout=120):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        st = json.loads(run("docker", "inspect", container_name).stdout)[0]["State"]
        if not st["Running"]:
            logs = run("docker", "logs", container_name, check=False).stderr
            raise AssertionError(f"container {container_name} exited early ({st['ExitCode']}):\n{logs}")
        try:
            _, body = http_req(base, "/status")
            return body
        except Exception as err:
            last = str(err)
        time.sleep(0.5)
    raise AssertionError(f"timed out waiting for {container_name} /status: {last}")


def drill(v5_image, v6_image, pg_image):
    suffix = uuid.uuid4().hex[:10]
    net = f"dhw-drill-{suffix}"
    pg = f"dhw-drill-pg-{suffix}"
    app = f"dhw-drill-app-{suffix}"
    run("docker", "network", "create", net)

    def psql(stmt):
        return run(
            "docker", "exec", "-i", pg,
            "psql", "-XqAt", "-v", "ON_ERROR_STOP=1", "-U", "bitmagnet", "-d", "bitmagnet",
            input_text=stmt,
        ).stdout.strip()

    def db_digest():
        snap = psql(
            "SELECT version_id, is_applied FROM goose_db_version ORDER BY id;"
            "SELECT key, value FROM key_values ORDER BY key;"
            "SELECT encode(info_hash,'hex'), name, private FROM torrents ORDER BY info_hash;"
        )
        return hashlib.sha256(snap.encode()).hexdigest()

    def base_env(extra=None):
        env = {
            "POSTGRES_HOST": pg,
            "POSTGRES_PORT": "5432",
            "POSTGRES_NAME": "bitmagnet",
            "POSTGRES_USER": "bitmagnet",
            "POSTGRES_PASSWORD": "drill-only-password",
            "PROCESSING_MODE": "basic",
            "CSAM_BLOCKLIST_ENABLED": "false",
            "CSAM_BLOCKLIST_EXPORT_ENABLED": "false",
            "TMDB_API_KEY": "",
            "TORZNAB_API_KEY": "drill-key",
            "TRACKER_ENABLED": "false",
            "LOG_LEVEL": "info",
        }
        if extra:
            env.update(extra)
        out = []
        for k, v in env.items():
            out.extend(["-e", f"{k}={v}"])
        return out

    worker_cmd = ["worker", "run", "--keys", "http_server", "--keys", "queue_server", "--keys", "processors"]

    try:
        run(
            "docker", "run", "-d", "--name", pg, "--network", net,
            "-e", "POSTGRES_DB=bitmagnet",
            "-e", "POSTGRES_USER=bitmagnet",
            "-e", "POSTGRES_PASSWORD=drill-only-password",
            pg_image,
        )
        for _ in range(40):
            if run("docker", "exec", pg, "pg_isready", "-U", "bitmagnet", "-d", "bitmagnet", check=False).returncode == 0:
                break
            time.sleep(0.5)
        else:
            raise AssertionError("PostgreSQL did not become ready")

        # 1. Seed v5.15.0 & capture doctor fingerprint
        run("docker", "run", "-d", "--name", app, "--network", net, "-p", "127.0.0.1:0:3333",
            *base_env(), v5_image, *worker_cmd)
        port = run("docker", "port", app, "3333/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        base = f"http://127.0.0.1:{port}"
        wait_ready(base, app)

        records = [
            {"InfoHash": PUBLIC_HASH, "Name": NAME, "Source": "upgrade-drill", "Size": 1234567, "Private": False},
            {"InfoHash": PRIVATE_HASH, "Name": NAME, "Source": "upgrade-drill", "Size": 1234567, "Private": True},
        ]
        _, imp = http_req(base, "/import", "\n".join(map(json.dumps, records)) + "\n")
        assert b"2 items imported" in imp, f"unexpected import response: {imp!r}"

        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            _, raw = http_req(base, "/api/v1/search?q=Waypoint%20Upgrade%20Drill")
            body = json.loads(raw)
            if len(body.get("items", [])) == 1 and body["items"][0]["info_hash"] == PUBLIC_HASH:
                break
            time.sleep(0.5)
        else:
            raise AssertionError("v5 fixture never became searchable")

        doc_out = run("docker", "exec", app, "dh-waypoint", "doctor", "--check-v6").stdout
        m = re.search(r"UPGRADE_CONFIRM_V6_BACKUP=([0-9a-f]{12})", doc_out)
        assert m, f"doctor --check-v6 did not emit fingerprint:\n{doc_out}"
        fingerprint = m.group(1)
        print(f"[1/8] ({pg_image}) v5.15.0 seeded & doctor --check-v6 fingerprint={fingerprint}", flush=True)

        run("docker", "rm", "-f", app)
        digest_before = db_digest()

        # 2. Boot v6 WITHOUT ack -> MUST exit 78 with ZERO DB mutations
        res_no_ack = run("docker", "run", "--rm", "--network", net, *base_env(), v6_image, *worker_cmd, check=False)
        assert res_no_ack.returncode == 78, f"expected exit 78 without ack, got {res_no_ack.returncode}\n{res_no_ack.stderr}"
        assert fingerprint in res_no_ack.stderr, f"hard-stop message missing fingerprint {fingerprint}:\n{res_no_ack.stderr}"
        assert db_digest() == digest_before, "v6 mutated DB during hard stop without ack!"
        print(f"[2/8] ({pg_image}) v6 hard-stop without ack -> exit 78 & 0 DB mutations VERIFIED", flush=True)

        # 3. Boot v6 WITH wrong ack -> MUST exit 78 with ZERO DB mutations
        res_bad_ack = run(
            "docker", "run", "--rm", "--network", net,
            *base_env({"UPGRADE_CONFIRM_V6_BACKUP": "000000000000"}),
            v6_image, *worker_cmd, check=False,
        )
        assert res_bad_ack.returncode == 78, f"expected exit 78 with wrong ack, got {res_bad_ack.returncode}"
        assert db_digest() == digest_before, "v6 mutated DB on wrong ack!"
        print(f"[3/8] ({pg_image}) v6 hard-stop with wrong ack -> exit 78 & 0 DB mutations VERIFIED", flush=True)

        # 4. Restricted non-superuser PG role succeeds on `v6 upgrade --check` (no pg_control_system superuser needed)
        psql("CREATE ROLE dhw_nonsuper LOGIN PASSWORD 'nonsuper-pw';"
             "GRANT USAGE ON SCHEMA public TO dhw_nonsuper;"
             "GRANT SELECT ON ALL TABLES IN SCHEMA public TO dhw_nonsuper;")
        res_ns = run(
            "docker", "run", "--rm", "--network", net,
            *base_env({"POSTGRES_USER": "dhw_nonsuper", "POSTGRES_PASSWORD": "nonsuper-pw"}),
            v6_image, "upgrade", "--check", check=False,
        )
        assert res_ns.returncode == 0, f"non-superuser upgrade --check failed ({res_ns.returncode}):\n{res_ns.stdout}\n{res_ns.stderr}"
        assert re.search(r"UPGRADE_CONFIRM_V6_BACKUP=[0-9a-f]{12}", res_ns.stdout), res_ns.stdout
        print(f"[4/8] ({pg_image}) non-superuser role `upgrade --check` fingerprint fallback VERIFIED", flush=True)

        # 5. Run CLI `dh-waypoint upgrade --confirm-backup=<fp>` and verify receipt + search parity!
        res_cli_upg = run(
            "docker", "run", "--rm", "--network", net, *base_env(),
            v6_image, "upgrade", f"--confirm-backup={fingerprint}",
        )
        assert "Upgrade complete" in res_cli_upg.stdout, res_cli_upg.stdout
        assert psql("SELECT value FROM key_values WHERE key='schema_major';") == "6"
        assert psql("SELECT COUNT(*) FROM goose_db_version WHERE version_id = 100 AND is_applied;") == "1"
        receipt = json.loads(psql("SELECT value FROM key_values WHERE key='v6_upgrade_receipt';"))
        assert receipt["db_fingerprint"] == fingerprint and receipt["pre_upgrade_goose_version"] == 83, receipt
        print(f"[5/8] ({pg_image}) CLI `upgrade --confirm-backup` -> single v100 row & receipt VERIFIED", flush=True)

        # 6. Start v6 WITHOUT UPGRADE_CONFIRM_V6_BACKUP -> seamless boot & search parity!
        run(
            "docker", "run", "-d", "--name", app, "--network", net, "-p", "127.0.0.1:0:3333",
            *base_env(), v6_image, *worker_cmd,
        )
        port = run("docker", "port", app, "3333/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        base = f"http://127.0.0.1:{port}"
        wait_ready(base, app)
        _, raw = http_req(base, "/api/v1/search?q=Waypoint%20Upgrade%20Drill")
        body = json.loads(raw)
        assert len(body["items"]) == 1 and body["items"][0]["info_hash"] == PUBLIC_HASH
        print(f"[6/8] ({pg_image}) v6 boot without ack env var -> seamless persistence & parity VERIFIED", flush=True)
        run("docker", "rm", "-f", app)

        # 7. Attempt v5.15.0 against upgraded v6 DB -> symmetric exit 78 refusal!
        res_down = run("docker", "run", "--rm", "--network", net, *base_env(), v5_image, *worker_cmd, check=False)
        assert res_down.returncode == 78, f"expected v5 downgrade refusal exit 78, got {res_down.returncode}\n{res_down.stderr}"
        print(f"[7/8] ({pg_image}) v5.15.0 symmetric downgrade refusal on v6 DB -> exit 78 VERIFIED", flush=True)

        # 8. Simulated pre-v5.12 DB (Goose 70) -> unconditional exit 78 even with Goose 70's OWN fingerprint!
        psql("DELETE FROM key_values WHERE key IN ('schema_major','v6_upgrade_receipt'); DELETE FROM goose_db_version WHERE version_id > 70;")
        chk_old = run("docker", "run", "--rm", "--network", net, *base_env(), v6_image, "upgrade", "--check", check=False)
        assert chk_old.returncode == 78, f"expected upgrade --check exit 78 on Goose 70, got {chk_old.returncode}"
        m70 = re.search(r"UPGRADE_CONFIRM_V6_BACKUP=([0-9a-f]{12})", chk_old.stdout)
        assert m70, f"could not extract Goose-70 fingerprint:\n{chk_old.stdout}"
        fp70 = m70.group(1)
        assert fp70 != fingerprint, "Goose-70 fingerprint should differ from Goose-83 fingerprint"
        res_old = run(
            "docker", "run", "--rm", "--network", net,
            *base_env({"UPGRADE_CONFIRM_V6_BACKUP": fp70}),
            v6_image, *worker_cmd, check=False,
        )
        assert res_old.returncode == 78, f"expected unconditional exit 78 on Goose 70 with own fp {fp70}, got {res_old.returncode}\n{res_old.stderr}"
        assert "below v6 direct-upgrade floor" in res_old.stderr, res_old.stderr
        assert psql("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied;") == "70"
        print(f"[8/8] ({pg_image}) pre-v5.12 (Goose 70) unconditional exit 78 WITH OWN fp={fp70} VERIFIED", flush=True)
    finally:
        run("docker", "rm", "-f", app, pg, check=False)
        run("docker", "network", "rm", net, check=False)


def compose_drill(v5_image, v6_image):
    """Real `docker compose` v5.15.0 -> v6.0 upgrade drill proving PG16 volume preservation
    and root-owned v5 config.yml API key preservation under non-root UID 1000:1000."""
    project = f"dhwcmp{uuid.uuid4().hex[:8]}"
    api_key = "compose-drill-secret-key-2026"
    tmp_dir = Path(tempfile.mkdtemp(prefix="dhw-compose-drill-"))
    (tmp_dir / "plugins").mkdir()
    env_file = tmp_dir / ".env"

    def write_env(extra=""):
        env_file.write_text(
            "POSTGRES_PASSWORD=compose-drill-pass\n"
            "TORZNAB_API_KEY=compose-torznab-key\n"
            "PROCESSORS_ADMIN_TOKEN=compose-admin-tok\n"
            "PROCESSING_MODE=basic\n"
            "CSAM_BLOCKLIST_ENABLED=false\n"
            "CSAM_BLOCKLIST_EXPORT_ENABLED=false\n"
            "TRACKER_ENABLED=false\n"
            "DHT_CRAWLER_ENABLED=false\n"
            "TMDB_ENABLED=false\n"
            + extra
        )

    v5_compose = tmp_dir / "v5-compose.yml"
    v5_compose.write_text(f"""services:
  dh-waypoint:
    image: {v5_image}
    command: ["worker", "run", "--keys", "http_server", "--keys", "queue_server", "--keys", "processors"]
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      POSTGRES_HOST: postgres
      POSTGRES_PORT: 5432
      POSTGRES_NAME: bitmagnet
      POSTGRES_USER: bitmagnet
      POSTGRES_PASSWORD: ${{POSTGRES_PASSWORD}}
      PROCESSING_MODE: basic
      CSAM_BLOCKLIST_ENABLED: "false"
      CSAM_BLOCKLIST_EXPORT_ENABLED: "false"
      TRACKER_ENABLED: "false"
    ports:
      - "127.0.0.1:0:3333"
    volumes:
      - dh-waypoint-config:/root/.config/bitmagnet
      - dh-waypoint-data:/root/.local/share/bitmagnet
      - ./plugins:/plugins:ro
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: bitmagnet
      POSTGRES_USER: bitmagnet
      POSTGRES_PASSWORD: ${{POSTGRES_PASSWORD}}
      PGUSER: bitmagnet
    volumes:
      - dh-waypoint-postgres:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U bitmagnet -d bitmagnet"]
      interval: 2s
      timeout: 3s
      retries: 20
volumes:
  dh-waypoint-config:
  dh-waypoint-data:
  dh-waypoint-postgres:
""")

    # Override ONLY local test image, dynamic ports, container_name collision removal, and non-DHT worker keys
    # while testing the REAL repository docker-compose.yml verbatim (PG image, mount targets, volumes, env).
    v6_override = tmp_dir / "v6-override.yml"
    v6_override.write_text(f"""services:
  dh-waypoint:
    image: {v6_image}
    container_name: {project}-app
    restart: "no"
    command: ["worker", "run", "--keys", "http_server", "--keys", "queue_server", "--keys", "processors"]
    ports: !override
      - "127.0.0.1:0:3333"
    environment:
      CSAM_BLOCKLIST_ENABLED: "false"
      CSAM_BLOCKLIST_EXPORT_ENABLED: "false"
      TRACKER_ENABLED: "false"
  postgres:
    container_name: {project}-pg
""")

    def compose(files, *args, check=True, input_text=None):
        cmd = ["docker", "compose", "-p", project, "--env-file", str(env_file)]
        for f in files:
            cmd.extend(["-f", str(f)])
        cmd.extend(args)
        return run(*cmd, check=check, timeout=180, input_text=input_text)

    def psql(files, stmt):
        return compose(files, "exec", "-T", "postgres", "psql", "-XqAt", "-v", "ON_ERROR_STOP=1", "-U", "bitmagnet", "-d", "bitmagnet", input_text=stmt).stdout.strip()

    def db_digest(files):
        snap = psql(
            files,
            "SELECT version_id, is_applied FROM goose_db_version ORDER BY id;"
            "SELECT key, value FROM key_values ORDER BY key;"
            "SELECT encode(info_hash,'hex'), name, private FROM torrents ORDER BY info_hash;",
        )
        return hashlib.sha256(snap.encode()).hexdigest()

    v5_files = [v5_compose]
    v6_files = [ROOT / "docker-compose.yml", v6_override]

    try:
        write_env()
        # Start postgres and populate root-owned v5 config.yml with http_server.api_key inside dh-waypoint-config
        compose(v5_files, "up", "-d", "postgres")
        cfg_vol = f"{project}_dh-waypoint-config"
        run(
            "docker", "run", "--rm", "-u", "0", "-v", f"{cfg_vol}:/cfg", v5_image,
            "--version", check=False,
        )
        run(
            "docker", "run", "--rm", "-u", "0", "--entrypoint", "/bin/sh",
            "-v", f"{cfg_vol}:/cfg", v5_image, "-ec",
            f"printf 'http_server:\\n  api_key: {api_key}\\n' > /cfg/config.yml && chmod 644 /cfg/config.yml && chown -R 0:0 /cfg",
        )

        # Boot v5.15.0 via Compose, verify API key enforced, import fixtures, get fingerprint
        compose(v5_files, "up", "-d", "dh-waypoint")
        cid = compose(v5_files, "ps", "-q", "dh-waypoint").stdout.strip()
        port = run("docker", "port", cid, "3333/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        base = f"http://127.0.0.1:{port}"
        wait_ready(base, cid)

        assert http_status(base, "/api/v1/search?q=Waypoint") == 401, "v5 config.yml api_key was not enforced!"
        records = [
            {"InfoHash": PUBLIC_HASH, "Name": NAME, "Source": "compose-drill", "Size": 1234567, "Private": False},
            {"InfoHash": PRIVATE_HASH, "Name": NAME, "Source": "compose-drill", "Size": 1234567, "Private": True},
        ]
        _, imp = http_req(base, "/import", "\n".join(map(json.dumps, records)) + "\n", headers={"X-API-Key": api_key})
        assert b"2 items imported" in imp, f"unexpected import response: {imp!r}"

        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            _, raw = http_req(base, "/api/v1/search?q=Waypoint%20Upgrade%20Drill", headers={"X-API-Key": api_key})
            body = json.loads(raw)
            if len(body.get("items", [])) == 1 and body["items"][0]["info_hash"] == PUBLIC_HASH:
                break
            time.sleep(0.5)
        else:
            raise AssertionError("v5 Compose fixture never became searchable")

        doc_out = compose(v5_files, "exec", "-T", "dh-waypoint", "dh-waypoint", "doctor", "--check-v6").stdout
        m = re.search(r"UPGRADE_CONFIRM_V6_BACKUP=([0-9a-f]{12})", doc_out)
        assert m, f"v5 Compose doctor --check-v6 missing fingerprint:\n{doc_out}"
        fp = m.group(1)
        compose(v5_files, "rm", "-sf", "dh-waypoint")
        digest_before = db_digest(v5_files)
        print(f"[COMPOSE 1/4] v5.15.0 stack seeded with root-owned config.yml API key; fp={fp}", flush=True)

        # Swap in real v6 docker-compose.yml WITHOUT ack -> MUST exit 78 with ZERO DB mutations!
        compose(v6_files, "up", "-d", "postgres")
        res_stop = compose(v6_files, "run", "--rm", "--no-deps", "dh-waypoint", "worker", "run", "--keys", "http_server", check=False)
        assert res_stop.returncode == 78, f"expected v6 Compose exit 78 without ack, got {res_stop.returncode}\nSTDOUT:\n{res_stop.stdout}\nSTDERR:\n{res_stop.stderr}"
        assert fp in res_stop.stderr, f"v6 Compose hard-stop missing fp {fp}:\n{res_stop.stderr}"
        assert db_digest(v6_files) == digest_before, "v6 Compose mutated DB during hard stop!"
        print("[COMPOSE 2/4] v6 docker-compose.yml on v5 PG16 volume -> exit 78 & 0 DB mutations VERIFIED", flush=True)

        # Pass UPGRADE_CONFIRM_V6_BACKUP in .env and start v6 via real docker-compose.yml
        write_env(f"UPGRADE_CONFIRM_V6_BACKUP={fp}\n")
        compose(v6_files, "up", "-d", "dh-waypoint")
        cid6 = compose(v6_files, "ps", "-q", "dh-waypoint").stdout.strip()
        port6 = run("docker", "port", cid6, "3333/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        base6 = f"http://127.0.0.1:{port6}"
        wait_ready(base6, cid6)

        assert psql(v6_files, "SELECT value FROM key_values WHERE key='schema_major';") == "6"
        assert psql(v6_files, "SELECT COUNT(*) FROM goose_db_version WHERE version_id = 100 AND is_applied;") == "1"
        print("[COMPOSE 3/4] v6 Compose automatic upgrade -> schema_major=6 & single v100 row VERIFIED", flush=True)

        # CRITICAL P0-B PROOF: v5 root-owned config.yml API key STILL enforced under non-root UID 1000!
        assert http_status(base6, "/api/v1/search?q=Waypoint") == 401, "REGRESSION: v5 config.yml api_key was ignored after v6 non-root remap!"
        st, raw6 = http_req(base6, "/api/v1/search?q=Waypoint%20Upgrade%20Drill", headers={"X-API-Key": api_key})
        assert st == 200
        body6 = json.loads(raw6)
        assert len(body6["items"]) == 1 and body6["items"][0]["info_hash"] == PUBLIC_HASH
        print("[COMPOSE 4/4] root-owned v5 config.yml API key enforced under non-root UID 1000 VERIFIED", flush=True)
        print("ALL COMPOSE v5.15 -> v6.0 UPGRADE & CONFIG-PRESERVATION CHECKS PASSED")
    finally:
        compose(v6_files, "down", "-v", "--remove-orphans", check=False)
        compose(v5_files, "down", "-v", "--remove-orphans", check=False)
        run("rm", "-rf", str(tmp_dir), check=False)


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--v5-image", required=True)
    ap.add_argument("--v6-image", required=True)
    ap.add_argument("--pg-image", default="postgres:16-alpine")
    ap.add_argument("--skip-compose", action="store_true")
    args = ap.parse_args()
    drill(args.v5_image, args.v6_image, args.pg_image)
    if not args.skip_compose:
        compose_drill(args.v5_image, args.v6_image)
