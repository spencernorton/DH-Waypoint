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
        current=json.loads(subprocess.check_output(["docker","inspect",args.container]))[0]
        if current['State']['Running']:
            pid=current['State']['Pid']
            try:
                relative=next(line.split(':',2)[2] for line in Path(f'/proc/{pid}/cgroup').read_text().splitlines() if line.startswith('0::'))
                cgroup=Path('/sys/fs/cgroup')/relative.lstrip('/')
                row['container_started_at']=current['State']['StartedAt']
                for name in ['memory.current','memory.peak']:
                    row[name]=int((cgroup/name).read_text())
                for name in ['memory.stat','cpu.stat','io.stat']:
                    row[name]=(cgroup/name).read_text()
            except (OSError,StopIteration,ValueError):
                row['cgroup_counters_unavailable']=True
        row["at"] = time.time()
        with open(args.out, "a") as f:
            f.write(json.dumps(row, sort_keys=True) + "\n")
    except (subprocess.CalledProcessError, json.JSONDecodeError):
        pass
    time.sleep(5)
