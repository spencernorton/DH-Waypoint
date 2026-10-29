#!/usr/bin/env python3
"""Qualify a built backend through its public Compose stack and real APIs.

Uses an empty disposable PostgreSQL volume, synthetic metadata and the shipped
regex plugin. No provider, model, public peer or production database is needed.
"""

import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
PUBLIC_HASH = "a1" * 20
PRIVATE_HASH = "b2" * 20
NAME = "Waypoint.Runtime.Fixture.2024.1080p.REMUX"
SEARCH = """query { torrentContent { search(input: {queryString: "Waypoint Runtime Fixture", limit: 10}) {
  items { infoHash torrent { tagNames } }
} } }"""


def run(*args, **kwargs):
    result = subprocess.run([str(arg) for arg in args], text=True, capture_output=True,
                            timeout=kwargs.pop("timeout", 60), **kwargs)
    if result.returncode:
        raise RuntimeError(f"{' '.join(map(str, args))}:\n{result.stdout}\n{result.stderr}")
    return result.stdout.strip()


def request(base, path, payload=None, content_type="application/json"):
    data = None if payload is None else payload.encode()
    req = urllib.request.Request(base + path, data, {"Content-Type": content_type})
    with urllib.request.urlopen(req, timeout=10) as response:
        return response.read()


def eventually(check, description, timeout=120):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = check()
            if last:
                return last
        except (urllib.error.URLError, TimeoutError, ConnectionError) as error:
            last = str(error)
        time.sleep(0.5)
    raise AssertionError(f"Timed out waiting for {description}: {last}")


def qualify(image, evidence):
    image_id = json.loads(run("docker", "image", "inspect", image))[0]["Id"]
    project = "waypoint-ci-" + uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix="waypoint-runtime-") as temporary:
        scratch = Path(temporary)
        environment = scratch / ".env"
        environment.write_text("POSTGRES_PASSWORD=ci-only-password\n")
        plugins = scratch / "plugins"
        shutil.copytree(ROOT / "plugins", plugins)
        regex = plugins / "regex-classifier.plugin.yml"
        source = regex.read_text()
        if "enabled: false" not in source:
            raise AssertionError("The shipped regex template's opt-in changed; review the runtime setup")
        regex.write_text(source.replace("enabled: false", "enabled: true", 1))

        # Resolve the actual quickstart first, then isolate only its resources.
        model = json.loads(run("docker", "compose", "--env-file", environment,
                               "-f", ROOT / "docker-compose.yml", "config", "--format", "json"))
        model.pop("name", None)
        model["services"] = {key: model["services"][key] for key in ("dh-waypoint", "postgres")}
        model["networks"] = {"default": {}}
        for volume in model.get("volumes", {}).values():
            volume.pop("name", None)
        for service in model["services"].values():
            service.pop("container_name", None)
            service["restart"] = "no"
            service["ports"] = []
        app = model["services"]["dh-waypoint"]
        app["image"] = image
        app["command"] = ["worker", "run", "--keys", "http_server", "--keys", "queue_server", "--keys", "processors"]
        app["ports"] = [{"target": 3333, "published": "0", "host_ip": "127.0.0.1"}]
        app["environment"].update({"PROCESSING_MODE": "basic", "CSAM_BLOCKLIST_ENABLED": "false",
                                   "CSAM_BLOCKLIST_EXPORT_ENABLED": "false", "TMDB_API_KEY": "",
                                   "TORZNAB_API_KEY": "ci-only-key", "EVIDENCE_WEBHOOK_SECRET": "",
                                   "CONTENT_FILTER_ENABLED": "false", "RETENTION_ENABLED": "false",
                                   "TRACKER_ENABLED": "false", "PROCESSORS_ADMIN_TOKEN": "", "LOG_LEVEL": "info"})
        for mount in app["volumes"]:
            if mount["target"] == "/plugins":
                mount["source"] = str(plugins)
        compose_file = scratch / "compose.json"
        compose_file.write_text(json.dumps(model))

        def compose(*args, **kwargs):
            return run("docker", "compose", "-p", project, "-f", compose_file, *args, **kwargs)

        def alive():
            container = compose("ps", "--all", "-q", "dh-waypoint")
            state = json.loads(run("docker", "inspect", container))[0]
            if not state["State"]["Running"]:
                raise AssertionError(f"Candidate exited: {state['State']}")
            if state["Image"] != image_id:
                raise AssertionError("Compose did not start the candidate image")
            return state

        def sql(statement):
            return compose("exec", "-T", "postgres", "psql", "-XqAt", "-v", "ON_ERROR_STOP=1",
                           "-U", "bitmagnet", "-d", "bitmagnet", input=statement)

        try:
            compose("up", "-d", timeout=180)
            base = "http://" + compose("port", "dh-waypoint", "3333")

            def ready():
                alive()
                return request(base, "/status")

            eventually(ready, "migrations and healthy backend", timeout=180)
            print("Empty-database migrations and health passed", flush=True)
            if sql("select count(*) from torrents;") != "0":
                raise AssertionError("Runtime qualification requires an empty database")
            workers = compose("exec", "-T", "dh-waypoint", "dh-waypoint", "plugin", "list")
            if "regex-classifier" not in workers:
                raise AssertionError("The shipped plugin was not registered")

            records = [{"InfoHash": hash_value, "Name": NAME, "Source": "runtime-fixture",
                        "Size": 12345678, "Private": private}
                       for hash_value, private in ((PUBLIC_HASH, False), (PRIVATE_HASH, True))]
            imported = request(base, "/import", "\n".join(map(json.dumps, records)) + "\n")
            if b"import complete" not in imported or b"2 items imported" not in imported:
                raise AssertionError(f"Public import did not complete: {imported!r}")

            def processed():
                alive()
                response = json.loads(request(base, "/api/v1/search?q=Waypoint%20Runtime%20Fixture"))
                items = response["items"]
                if len(items) != 1 or items[0]["info_hash"] != PUBLIC_HASH:
                    return None
                if items[0].get("content_type") != "movie":
                    return None
                tags = sql(f"select name from torrent_tags where info_hash=decode('{PUBLIC_HASH}','hex');")
                return response if "remux" in tags.splitlines() else None

            search = eventually(processed, "import → queue → regex plugin → persisted movie/remux → REST search")
            print("Import, queue, regex classification and REST persistence passed", flush=True)
            detail = json.loads(request(base, f"/api/v1/torrents/{PUBLIC_HASH}"))
            graph = json.loads(request(base, "/graphql", json.dumps({"query": SEARCH})))
            if graph.get("errors"):
                raise AssertionError(f"GraphQL errors: {graph['errors']}")
            graph_items = graph["data"]["torrentContent"]["search"]["items"]
            if len(graph_items) != 1 or graph_items[0]["infoHash"] != PUBLIC_HASH or "remux" not in graph_items[0]["torrent"]["tagNames"]:
                raise AssertionError(f"GraphQL lost the processed public fixture or exposed private data: {graph}")
            path = "/torznab?" + urllib.parse.urlencode({"t": "search", "q": "Waypoint Runtime Fixture", "apikey": "ci-only-key"})
            feed = ET.fromstring(request(base, path))
            feed_items = feed.findall("./channel/item")
            if len(feed_items) != 1 or PUBLIC_HASH not in ET.tostring(feed_items[0], encoding="unicode"):
                raise AssertionError("Torznab did not return exactly the processed public fixture")
            try:
                request(base, "/torznab?t=search&q=Waypoint")
            except urllib.error.HTTPError as error:
                if error.code not in (401, 403):
                    raise
            else:
                raise AssertionError("Torznab accepted a search without the configured API key")
            if sql(f"select private from torrents where info_hash=decode('{PRIVATE_HASH}','hex');") != "t":
                raise AssertionError("The private visibility check requires a persisted private fixture")

            # Restart the real candidate against the same volume; catalogue data
            # and migrations must survive a normal application restart.
            compose("restart", "dh-waypoint")
            # Docker can allocate a new ephemeral host port on stop/start.
            base = "http://" + compose("port", "dh-waypoint", "3333")
            eventually(ready, "healthy backend after restart", timeout=180)
            if not processed():
                raise AssertionError("Processed metadata did not survive restart")
            proof = {"image_id": image_id, "checks": ["empty-database migrations", "public import",
                     "queue processing", "regex classification and tag persistence", "REST", "GraphQL",
                     "Torznab search and authentication", "private fixture excluded", "restart persistence"],
                     "search": search, "detail": detail, "graphql": graph}
            (evidence / "runtime.json").write_text(json.dumps(proof, indent=2) + "\n")
            print(json.dumps({"image_id": image_id, "checks": proof["checks"]}, indent=2), flush=True)
        finally:
            try:
                (evidence / "compose.log").write_text(compose("logs", "--no-color", timeout=20))
                (evidence / "containers.json").write_text(compose("ps", "--all", "--format", "json"))
            finally:
                compose("down", "--volumes", "--remove-orphans", "--timeout", "5", timeout=60)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    qualify(args.image, args.evidence)
