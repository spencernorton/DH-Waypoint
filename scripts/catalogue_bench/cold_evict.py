#!/usr/bin/env python3
"""Evict only a stopped, explicitly labeled synthetic container's DB files.

Run with permissions to read the dedicated Docker volume (usually sudo -n).
Never drops global caches. Storage-device caches are outside this measurement.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess

p = argparse.ArgumentParser(description=__doc__)
p.add_argument("container")
args = p.parse_args()
container = json.loads(subprocess.check_output(["docker", "inspect", args.container]))[0]
if container["State"]["Running"]:
    raise SystemExit("refusing cache eviction while database is running")
if container["Config"].get("Labels", {}).get("purpose") != "disposable-synthetic-benchmark":
    raise SystemExit("refusing unlabeled container")
volumes = [m for m in container["Mounts"] if m["Destination"] == "/var/lib/postgresql/data" and m["Type"] == "volume"]
if len(volumes) != 1 or not volumes[0]["Name"].startswith("dh-waypoint-catalogue-bench-"):
    raise SystemExit("refusing non-benchmark volume")
root = Path(volumes[0]["Source"])
if (root / "PG_VERSION").read_text().strip() != "16":
    raise SystemExit("unexpected PostgreSQL data directory")
count = total = 0
for directory, _, files in os.walk(root):
    for name in files:
        file = Path(directory) / name
        if file.is_symlink() or not file.is_file():
            continue
        fd = os.open(file, os.O_RDONLY)
        try:
            os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED)
            count += 1
            total += os.fstat(fd).st_size
        finally:
            os.close(fd)
print(json.dumps({"kind": "file_cache_eviction", "files": count, "bytes": total}))
