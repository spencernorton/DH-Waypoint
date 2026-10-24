#!/usr/bin/env python3
"""Record resource counters of one labeled disposable benchmark container."""
import argparse
import json
from pathlib import Path
import subprocess
import time

p = argparse.ArgumentParser(description=__doc__)
p.add_argument("container")
p.add_argument("out")
args = p.parse_args()
metadata = json.loads(subprocess.check_output(["docker", "inspect", args.container]))[0]
if metadata["Config"].get("Labels", {}).get("purpose") != "disposable-synthetic-benchmark":
    raise SystemExit("refusing unlabeled container")
Path(args.out).parent.mkdir(parents=True, exist_ok=True)
while True:
    try:
        data = json.loads(subprocess.check_output(["docker", "stats", "--no-stream", "--format", "{{json .}}", args.container]))
        row = {key: data[key] for key in ["CPUPerc", "MemUsage", "MemPerc", "BlockIO", "PIDs"]}
        row["at"] = time.time()
        with open(args.out, "a") as f:
            f.write(json.dumps(row, sort_keys=True) + "\n")
    except (subprocess.CalledProcessError, json.JSONDecodeError):
        pass
    time.sleep(5)
