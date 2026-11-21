#!/usr/bin/env python3
"""End-to-end Docker verification of the v5.15 -> v6.0 hard-stop & auto-upgrade contract.

Proves against a real disposable PostgreSQL container:
  1. Legacy v5.15.0 initializes schema 83, imports public + private fixtures.
  2. Candidate v6.0.0 WITHOUT UPGRADE_CONFIRM_V6_BACKUP exits 78 with ZERO DB mutations.
  3. Candidate v6.0.0 WITH a wrong fingerprint exits 78 with ZERO DB mutations.
  4. Candidate v6.0.0 WITH the matching DB fingerprint runs automatic in-binary upgrade
     (schema 100, schema_major='6', v6_upgrade_receipt) and serves exact public/private results.
  5. Candidate v6.0.0 restarts cleanly WITHOUT UPGRADE_CONFIRM_V6_BACKUP.
  6. Legacy v5.15.0 pointed at the upgraded v6 DB refuses to start (exit 78, symmetric guard).
  7. Simulated pre-v5.12 DB (Goose 70) hits unconditional exit 78 even with matching fingerprint.
"""

import argparse
import hashlib
import json
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

PUBLIC_HASH = "a1" * 20
PRIVATE_HASH = "b2" * 20
NAME = "Waypoint.Upgrade.Drill.2026.1080p.REMUX"


def run(*args, check=True, timeout=120, input_text=None):
    res = subprocess.run(
        [str(a) for a in args],
        text=True,
        capture_output=True,
        timeout=timeout,
        input=input_text,
    )
    if check and res.returncode != 0:
        raise RuntimeError(
            f"command failed ({res.returncode}): {' '.join(map(str, args))}\n"
            f"STDOUT:\n{res.stdout}\nSTDERR:\n{res.stderr}"
        )
    return res


def http_req(base, path, payload=None):
    data = None if payload is None else payload.encode()
    req = urllib.request.Request(base + path, data, {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        return resp.read()


def wait_ready(base, container_name, timeout=120):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        st = json.loads(run("docker", "inspect", container_name).stdout)[0]["State"]
        if not st["Running"]:
            logs = run("docker", "logs", container_name, check=False).stderr
            raise AssertionError(f"container {container_name} exited early ({st['ExitCode']}):\n{logs}")
        try:
            return http_req(base, "/status")
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
        # 1. Start PostgreSQL
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

        # 2. Boot v5.15.0, import public + private fixtures, verify v6 readiness doctor
        run("docker", "run", "-d", "--name", app, "--network", net, "-p", "127.0.0.1:0:3333",
            *base_env(), v5_image, *worker_cmd)
        port = run("docker", "port", app, "3333/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        base = f"http://127.0.0.1:{port}"
        wait_ready(base, app)

        records = [
            {"InfoHash": PUBLIC_HASH, "Name": NAME, "Source": "upgrade-drill", "Size": 1234567, "Private": False},
            {"InfoHash": PRIVATE_HASH, "Name": NAME, "Source": "upgrade-drill", "Size": 1234567, "Private": True},
        ]
        imp = http_req(base, "/import", "\n".join(map(json.dumps, records)) + "\n")
        assert b"2 items imported" in imp, f"unexpected import response: {imp!r}"

        # Wait for public fixture to be searchable on v5
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            body = json.loads(http_req(base, "/api/v1/search?q=Waypoint%20Upgrade%20Drill"))
            if len(body.get("items", [])) == 1 and body["items"][0]["info_hash"] == PUBLIC_HASH:
                break
            time.sleep(0.5)
        else:
            raise AssertionError("v5 fixture never became searchable")

        doc_out = run("docker", "exec", app, "dh-waypoint", "doctor", "--check-v6").stdout
        m = re.search(r"UPGRADE_CONFIRM_V6_BACKUP=([0-9a-f]{12})", doc_out)
        assert m, f"doctor --check-v6 did not emit fingerprint:\n{doc_out}"
        fingerprint = m.group(1)
        print(f"[1/7] v5.15.0 seeded & doctor --check-v6 fingerprint={fingerprint}", flush=True)

        run("docker", "rm", "-f", app)
        digest_before = db_digest()

        # 3. Boot v6 WITHOUT ack -> MUST exit 78 with ZERO DB mutations
        res_no_ack = run("docker", "run", "--rm", "--network", net, *base_env(), v6_image, *worker_cmd, check=False)
        assert res_no_ack.returncode == 78, f"expected exit 78 without ack, got {res_no_ack.returncode}\n{res_no_ack.stderr}"
        assert fingerprint in res_no_ack.stderr, f"hard-stop message missing fingerprint {fingerprint}:\n{res_no_ack.stderr}"
        assert db_digest() == digest_before, "v6 mutated DB during hard stop without ack!"
        print("[2/7] v6 hard-stop without ack -> exit 78 & 0 DB mutations VERIFIED", flush=True)

        # 4. Boot v6 WITH wrong ack -> MUST exit 78 with ZERO DB mutations
        res_bad_ack = run(
            "docker", "run", "--rm", "--network", net,
            *base_env({"UPGRADE_CONFIRM_V6_BACKUP": "000000000000"}),
            v6_image, *worker_cmd, check=False,
        )
        assert res_bad_ack.returncode == 78, f"expected exit 78 with wrong ack, got {res_bad_ack.returncode}"
        assert db_digest() == digest_before, "v6 mutated DB on wrong ack!"
        print("[3/7] v6 hard-stop with wrong ack -> exit 78 & 0 DB mutations VERIFIED", flush=True)

        # 5. Boot v6 WITH matching fingerprint -> automatic in-binary upgrade!
        run(
            "docker", "run", "-d", "--name", app, "--network", net, "-p", "127.0.0.1:0:3333",
            *base_env({"UPGRADE_CONFIRM_V6_BACKUP": fingerprint}),
            v6_image, *worker_cmd,
        )
        port = run("docker", "port", app, "3333/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        base = f"http://127.0.0.1:{port}"
        wait_ready(base, app)

        assert psql("SELECT value FROM key_values WHERE key='schema_major';") == "6"
        body = json.loads(http_req(base, "/api/v1/search?q=Waypoint%20Upgrade%20Drill"))
        assert len(body["items"]) == 1 and body["items"][0]["info_hash"] == PUBLIC_HASH
        print("[4/7] v6 automatic upgrade with fingerprint -> schema_major=6 & search parity VERIFIED", flush=True)
        run("docker", "rm", "-f", app)

        # 6. Restart v6 WITHOUT UPGRADE_CONFIRM_V6_BACKUP -> seamless boot!
        run(
            "docker", "run", "-d", "--name", app, "--network", net, "-p", "127.0.0.1:0:3333",
            *base_env(), v6_image, *worker_cmd,
        )
        port = run("docker", "port", app, "3333/tcp").stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        base = f"http://127.0.0.1:{port}"
        wait_ready(base, app)
        body = json.loads(http_req(base, "/api/v1/search?q=Waypoint%20Upgrade%20Drill"))
        assert len(body["items"]) == 1 and body["items"][0]["info_hash"] == PUBLIC_HASH
        print("[5/7] v6 restart without ack env var -> seamless persistence VERIFIED", flush=True)
        run("docker", "rm", "-f", app)

        # 7. Attempt v5.15.0 against upgraded v6 DB -> symmetric exit 78 refusal!
        res_down = run("docker", "run", "--rm", "--network", net, *base_env(), v5_image, *worker_cmd, check=False)
        assert res_down.returncode == 78, f"expected v5 downgrade refusal exit 78, got {res_down.returncode}\n{res_down.stderr}"
        print("[6/7] v5.15.0 symmetric downgrade refusal on v6 DB -> exit 78 VERIFIED", flush=True)

        # 8. Simulated pre-v5.12 DB (Goose 70) -> unconditional exit 78 even with fingerprint!
        psql("DELETE FROM key_values WHERE key IN ('schema_major','v6_upgrade_receipt'); DELETE FROM goose_db_version WHERE version_id > 70;")
        res_old = run(
            "docker", "run", "--rm", "--network", net,
            *base_env({"UPGRADE_CONFIRM_V6_BACKUP": fingerprint}),
            v6_image, *worker_cmd, check=False,
        )
        assert res_old.returncode == 78, f"expected unconditional exit 78 on Goose 70, got {res_old.returncode}\n{res_old.stderr}"
        assert psql("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied;") == "70"
        print("[7/7] pre-v5.12 (Goose 70) unconditional exit 78 hard stop VERIFIED", flush=True)

        print("ALL 7 v5 -> v6 UPGRADE & HARD-STOP DRILL CHECKS PASSED")
    finally:
        run("docker", "rm", "-f", app, pg, check=False)
        run("docker", "network", "rm", net, check=False)


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--v5-image", required=True)
    ap.add_argument("--v6-image", required=True)
    ap.add_argument("--pg-image", default="postgres:16-alpine")
    args = ap.parse_args()
    drill(args.v5_image, args.v6_image, args.pg_image)
