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


def compose_drill(v5_image, v6_image, tms_image=None):
    """Real `docker compose` v5.15.0 -> v6.0 upgrade drill proving PG16 volume preservation
    and root-owned v5 config.yml API key preservation under non-root UID 1000:1000."""
    project = f"dhwcmp{uuid.uuid4().hex[:8]}"
    api_key = "compose-drill-secret-key-2026"
    tmp_dir = Path(tempfile.mkdtemp(prefix="dhw-compose-drill-"))
    (tmp_dir / "plugins").mkdir()
    env_file = tmp_dir / ".env"

    tms_token = f"compose-tms-{uuid.uuid4().hex[:12]}"

    def write_env(extra=""):
        tms_lines = (
            f"DH_TMS_AUTH_TOKEN={tms_token}\n"
            "DH_TMS_SYNC_INTERVAL_MS=500\n"
            if tms_image
            else "DH_TMS_READ_MODE=off\n"
        )
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
            + tms_lines
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
    tms_override_block = (
        f"""  dh-tmsd:
    image: {tms_image}
    container_name: {project}-tmsd
    restart: "no"
"""
        if tms_image
        else f"""  dh-tmsd:
    profiles: [disabled]
    container_name: {project}-tmsd
"""
    )
    tms_worker_keys = (
        '["worker", "run", "--keys", "http_server", "--keys", "queue_server", "--keys", "processors", "--keys", "dhtms_sync"]'
        if tms_image
        else '["worker", "run", "--keys", "http_server", "--keys", "queue_server", "--keys", "processors"]'
    )
    v6_override = tmp_dir / "v6-override.yml"
    deps_override = (
        ""
        if tms_image
        else """    depends_on: !override
      postgres:
        condition: service_healthy
"""
    )
    v6_override.write_text(f"""services:
  dh-waypoint:
    image: {v6_image}
    container_name: {project}-app
    restart: "no"
{deps_override}    command: {tms_worker_keys}
    ports: !override
      - "127.0.0.1:0:3333"
    environment:
      CSAM_BLOCKLIST_ENABLED: "false"
      CSAM_BLOCKLIST_EXPORT_ENABLED: "false"
      TRACKER_ENABLED: "false"
      DH_TMS_SYNC_INTERVAL_MS: "500"
  postgres:
    container_name: {project}-pg
{tms_override_block}""")

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
        if tms_image:
            # Wait for dhtms_sync to auto-bootstrap dh-tmsd from the upgraded PostgreSQL DB (gen >= 1, lease active).
            tms_cid = compose(v6_files, "ps", "-q", "dh-tmsd").stdout.strip()
            assert tms_cid, "default-on dh-tmsd service did not start in 3-service Compose stack!"
            deadline = time.monotonic() + 60
            tms_health = {}
            while time.monotonic() < deadline:
                h_res = run("docker", "exec", tms_cid, "curl", "-sf", "http://127.0.0.1:7841/v1/health", check=False)
                if h_res.returncode == 0 and h_res.stdout.strip():
                    tms_health = json.loads(h_res.stdout)
                    if tms_health.get("generation_id", 0) >= 1 and tms_health.get("lease_active") is True and tms_health.get("num_docs", 0) >= 2:
                        break
                time.sleep(0.5)
            else:
                logs_app = run("docker", "logs", cid6, check=False)
                raise AssertionError(f"dhtms_sync did not auto-bootstrap dh-tmsd: health={tms_health}\nAPP LOGS:\n{logs_app.stderr}")

        st, raw6 = http_req(base6, "/api/v1/search?q=Waypoint%20Upgrade%20Drill", headers={"X-API-Key": api_key})
        assert st == 200
        body6 = json.loads(raw6)
        assert len(body6["items"]) == 1 and body6["items"][0]["info_hash"] == PUBLIC_HASH

        if tms_image:
            st_tz, raw_tz = http_req(base6, "/torznab/api?t=search&q=Waypoint%20Upgrade%20Drill&apikey=compose-torznab-key")
            assert st_tz == 200 and PUBLIC_HASH.encode() in raw_tz and PRIVATE_HASH.encode() not in raw_tz, f"unexpected torznab response: {raw_tz[:400]!r}"
            _, metrics_raw = http_req(base6, "/metrics", headers={"X-API-Key": api_key})
            metrics_txt = metrics_raw.decode("utf-8", errors="replace")
            assert 'dh_waypoint_dhtms_reads_total{outcome="hit",surface="rest_v1"}' in metrics_txt, f"REST v1 did not serve from dh-tmsd!\n{metrics_txt}"
            assert 'dh_waypoint_dhtms_reads_total{outcome="hit",surface="torznab"}' in metrics_txt, f"Torznab did not serve from dh-tmsd!\n{metrics_txt}"
            print(f"[COMPOSE 4/4] root-owned v5 config.yml API key + automatic 3-service dh-tmsd sync (gen={tms_health['generation_id']}, docs={tms_health['num_docs']}) & live REST+Torznab serving VERIFIED", flush=True)
        else:
            print("[COMPOSE 4/4] root-owned v5 config.yml API key enforced under non-root UID 1000 VERIFIED", flush=True)
        print("ALL COMPOSE v5.15 -> v6.0 UPGRADE & CONFIG-PRESERVATION CHECKS PASSED")
    finally:
        compose(v6_files, "down", "-v", "--remove-orphans", check=False)
        compose(v5_files, "down", "-v", "--remove-orphans", check=False)
        run("rm", "-rf", str(tmp_dir), check=False)



def tms_compose_smoke(tms_image):
    """Verify the real docker-compose.yml dh-tmsd companion service (non-root UID 1000,
    internal-only network with zero published host ports, healthcheck, and token auth)."""
    project = f"dhwtms{uuid.uuid4().hex[:8]}"
    token = f"drill-{uuid.uuid4().hex[:12]}"
    wrong_token = f"bad-{uuid.uuid4().hex[:12]}"
    tmp_dir = Path(tempfile.mkdtemp(prefix="dhw-tms-compose-"))
    env_file = tmp_dir / ".env"
    env_file.write_text(
        "POSTGRES_PASSWORD=compose-tms-pass\n"
        f"DH_TMS_AUTH_TOKEN={token}\n"
    )
    override = tmp_dir / "tms-override.yml"
    override.write_text(f"""services:
  dh-tmsd:
    image: {tms_image}
    container_name: {project}-tmsd
    restart: "no"
""")
    files = [ROOT / "docker-compose.yml", override]

    def compose(*args, check=True):
        cmd = ["docker", "compose", "-p", project, "--env-file", str(env_file)]
        for f in files:
            cmd.extend(["-f", str(f)])
        cmd.extend(args)
        return run(*cmd, check=check, timeout=180)

    try:
        # 1. Boot companion dh-tmsd on a brand-new EMPTY volume -> healthy gen=0, fail-closed 503 before sync!
        compose("up", "-d", "dh-tmsd")
        cid = compose("ps", "-q", "dh-tmsd").stdout.strip()
        assert cid, "dh-tmsd container did not start on empty volume"
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            status = run("docker", "inspect", "--format", "{{.State.Health.Status}}", cid).stdout.strip()
            if status == "healthy":
                break
            time.sleep(0.5)
        else:
            logs = run("docker", "logs", cid, check=False)
            raise AssertionError(f"dh-tmsd unhealthy on empty volume:\n{logs.stdout}\n{logs.stderr}")
        empty_health = json.loads(run("docker", "exec", cid, "curl", "-sf", "http://127.0.0.1:7841/v1/health").stdout)
        assert empty_health.get("generation_id") == 0 and empty_health.get("lease_active") is False, f"unexpected empty health: {empty_health}"
        net_name = f"{project}_default"
        empty_probe = run(
            "docker", "run", "--rm", "--network", net_name,
            "-e", f"TMS_VALID={token}",
            "--entrypoint", "/bin/sh", tms_image, "-ec",
            """code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H "X-DHTMS-Token: $TMS_VALID" -d '{"query":"usual","tier":"base"}' http://dh-tmsd:7841/v1/search); test "$code" = 503""",
        )
        assert empty_probe.returncode == 0
        print("[TMS 1/4] empty-volume boot healthy (gen=0, lease_active=false) & fail-closed 503 VERIFIED", flush=True)

        # 2. Non-root UID 1000 initializes a synthetic generation and restarts dh-tmsd
        compose("stop", "dh-tmsd")
        init_res = compose("run", "--rm", "dh-tmsd", "init-fixture", "--out", "/var/lib/dh-tms/db")
        manifest = json.loads(init_res.stdout)
        assert manifest.get("generation_id"), f"unexpected init-fixture manifest: {manifest}"
        compose("up", "-d", "dh-tmsd")
        cid = compose("ps", "-q", "dh-tmsd").stdout.strip()
        assert cid, "dh-tmsd container did not start"
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            status = run("docker", "inspect", "--format", "{{.State.Health.Status}}", cid).stdout.strip()
            if status == "healthy":
                break
            if status == "unhealthy":
                logs = run("docker", "logs", cid, check=False)
                raise AssertionError(f"dh-tmsd unhealthy:\n{logs.stdout}\n{logs.stderr}")
            time.sleep(0.5)
        else:
            logs = run("docker", "logs", cid, check=False)
            raise AssertionError(f"dh-tmsd never became healthy:\n{logs.stdout}\n{logs.stderr}")
        print("[TMS 2/4] companion dh-tmsd Compose healthcheck healthy VERIFIED", flush=True)

        # 3. Verify zero published host ports and UID 1000 process ownership
        ports = run("docker", "port", cid).stdout.strip()
        assert ports == "", f"dh-tmsd must not publish host ports, got: {ports}"
        uid = run("docker", "exec", cid, "id", "-u").stdout.strip()
        assert uid == "1000", f"expected non-root UID 1000, got {uid}"
        print("[TMS 3/4] zero published host ports & UID 1000 runtime VERIFIED", flush=True)

        # 4. Verify internal Compose network reachability + 401/200 X-DHTMS-Token enforcement
        net_name = f"{project}_default"
        payload = '{"query":"usual suspects","tier":"base"}'
        probe_script = (
            "set -eu\n"
            "code_health=$(curl -s -o /dev/null -w '%{http_code}' http://dh-tmsd:7841/v1/health)\n"
            'test "$code_health" = 200\n'
            f"code_unauth=$(curl -s -o /dev/null -w '%{{http_code}}' -X POST -H 'Content-Type: application/json' -d '{payload}' http://dh-tmsd:7841/v1/search)\n"
            'test "$code_unauth" = 401\n'
            f"code_wrong=$(curl -s -o /dev/null -w '%{{http_code}}' -X POST -H 'Content-Type: application/json' -H \"X-DHTMS-Token: $TMS_WRONG\" -d '{payload}' http://dh-tmsd:7841/v1/search)\n"
            'test "$code_wrong" = 401\n'
            f"code_ok=$(curl -s -o /tmp/out.json -w '%{{http_code}}' -X POST -H 'Content-Type: application/json' -H \"X-DHTMS-Token: $TMS_VALID\" -d '{payload}' http://dh-tmsd:7841/v1/search)\n"
            'test "$code_ok" = 200\n'
            "cat /tmp/out.json\n"
        )
        probe = run(
            "docker", "run", "--rm", "--network", net_name,
            "-e", f"TMS_VALID={token}",
            "-e", f"TMS_WRONG={wrong_token}",
            "--entrypoint", "/bin/sh", tms_image, "-ec", probe_script,
        )
        search_resp = json.loads(probe.stdout)
        assert isinstance(search_resp.get("docs"), list) and len(search_resp["docs"]) >= 1, f"expected >=1 fixture doc, got: {search_resp}"
        print(f"[TMS 4/4] internal network auth 401/401/200 & fixture search docs ({len(search_resp['docs'])}) VERIFIED", flush=True)
        print("ALL DH-TMS COMPANION COMPOSE CHECKS PASSED")
    finally:
        compose("down", "-v", "--remove-orphans", check=False)
        run("rm", "-rf", str(tmp_dir), check=False)


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--v5-image")
    ap.add_argument("--v6-image")
    ap.add_argument("--pg-image", default="postgres:16-alpine")
    ap.add_argument("--skip-compose", action="store_true")
    ap.add_argument("--tms-image")
    ap.add_argument("--tms-only", action="store_true")
    args = ap.parse_args()
    if args.tms_only:
        if not args.tms_image:
            ap.error("--tms-image is required with --tms-only")
        tms_compose_smoke(args.tms_image)
    else:
        if not args.v5_image or not args.v6_image:
            ap.error("--v5-image and --v6-image are required unless --tms-only is set")
        drill(args.v5_image, args.v6_image, args.pg_image)
        if not args.skip_compose:
            compose_drill(args.v5_image, args.v6_image, tms_image=args.tms_image)
        if args.tms_image:
            tms_compose_smoke(args.tms_image)
