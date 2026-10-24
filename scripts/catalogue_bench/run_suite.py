#!/usr/bin/env python3
"""Run the same disposable workload at increasing scales; persist every stage.

Needs Linux, Docker, the psycopg venv and sudo -n for file-cache cold trials.
Supply an existing container labeled purpose=disposable-synthetic-benchmark.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

import psycopg

import bench

p=argparse.ArgumentParser(description=__doc__)
p.add_argument("container")
p.add_argument("--out",required=True)
p.add_argument("--sizes",default="2400000,10000000,32000000")
p.add_argument("--samples",type=int,default=30)
p.add_argument("--cold-samples",type=int,default=10)
p.add_argument("--skip-seed",action="store_true")
p.add_argument("--variants",default="baseline,candidate")
args=p.parse_args()
out=Path(args.out).resolve()
out.mkdir(parents=True,exist_ok=True)
root=Path(__file__).resolve().parent
metadata=json.loads(subprocess.check_output(["docker","inspect",args.container]))[0]
if metadata["Config"].get("Labels",{}).get("purpose")!="disposable-synthetic-benchmark":
    raise SystemExit("refusing unlabeled database")
if metadata["HostConfig"]["Memory"]>20*2**30 or not 0<metadata["HostConfig"]["NanoCpus"]<=8*10**9:
    raise SystemExit("benchmark container must be capped at <=8 CPUs and <=20 GiB RAM")
bench.emit(out/"suite.jsonl",{"kind":"manifest","format":2,"image":metadata["Image"],
 "memory_limit_bytes":metadata["HostConfig"]["Memory"],"nano_cpus":metadata["HostConfig"]["NanoCpus"],
 "source_sha256":{f.name:hashlib.sha256(f.read_bytes()).hexdigest() for f in root.glob("*.py")},
 "sizes":[int(x) for x in args.sizes.split(',')],"samples":args.samples,"cold_samples":args.cold_samples})


def stage(name,command):
    start=time.monotonic()
    bench.emit(out/"suite.jsonl",{"kind":"stage_start","stage":name,
      "bench_sha256":hashlib.sha256((root/'bench.py').read_bytes()).hexdigest()})
    with (out/(name+".log")).open("a") as log:
        result=subprocess.run(command,stdout=log,stderr=subprocess.STDOUT)
    bench.emit(out/"suite.jsonl",{"kind":"stage_end","stage":name,"seconds":time.monotonic()-start,"exit_code":result.returncode})
    if result.returncode:
        raise SystemExit(f"stage failed: {name} (see preserved log)")


def run(name,command,*extra):
    stage(name,[sys.executable,str(root/"bench.py"),command,"--out",str(out/(name+".jsonl")),*extra])


def start_database():
    subprocess.run(["docker","start",args.container],check=True,stdout=subprocess.DEVNULL)
    deadline=time.monotonic()+40
    while time.monotonic()<deadline:
        try:
            with bench.connect(os.environ["CATALOGUE_BENCH_DSN"]) as conn:
                conn.execute("SELECT 1")
            return
        except psycopg.OperationalError:
            time.sleep(0.2)
    raise SystemExit("disposable database did not become ready")


for size in [int(x) for x in args.sizes.split(',')]:
    if size>32000000:
        raise SystemExit("size outside qualified workload bounds")
    run(f"{size}-remove-candidates","indexes","--variant","baseline")
    if not args.skip_seed:
        run(f"{size}-seed","seed","--rows",str(size),"--workers","4","--disposable")
    for variant in args.variants.split(','):
        if variant not in ["baseline","candidate"]:
            raise SystemExit("invalid variant")
        run(f"{size}-{variant}-indexes","indexes","--variant",variant)
        run(f"{size}-{variant}-warm","run","--variant",variant,"--samples",str(args.samples))
        run(f"{size}-{variant}-concurrent","run","--variant",variant,"--samples",str(args.samples),"--workers","4","--updates","--ingest")
        for case in ["hash","identity_rare","external_keyset","type_browse"]:
            for sample in range(args.cold_samples):
                subprocess.run(["docker","stop","--time","30",args.container],check=True,stdout=subprocess.DEVNULL)
                try:
                    stage(f"{size}-{variant}-cold-{case}-{sample}-evict",
                          ["sudo","-n",sys.executable,str(root/"cold_evict.py"),args.container])
                finally:
                    start_database()
                run(f"{size}-{variant}-cold-{case}-{sample}","run","--variant",variant,
                    "--samples","1","--cache-state","first_touch","--cases",case,"--rows",str(size))
        run(f"{size}-{variant}-reindex","reindex","--variant",variant)
    with bench.connect(os.environ["CATALOGUE_BENCH_DSN"]) as conn:
        before=conn.execute("SELECT count(*) FROM torrents").fetchone()[0]
    # A deliberate crash is confined to this disposable container/volume.
    subprocess.run(["docker","kill","--signal","KILL",args.container],check=True,stdout=subprocess.DEVNULL)
    start=time.monotonic()
    start_database()
    with bench.connect(os.environ["CATALOGUE_BENCH_DSN"]) as conn:
        after=conn.execute("SELECT count(*) FROM torrents").fetchone()[0]
        sentinel=conn.execute("SELECT format FROM sentinel").fetchone()[0]
    bench.emit(out/"suite.jsonl",{"kind":"crash_recovery","rows":size,"before":before,"after":after,
               "sentinel":sentinel,"ready_and_count_seconds":time.monotonic()-start})
    if before!=after or sentinel!="synthetic-catalogue-v1":
        raise SystemExit("recovery check failed")
    bench.emit(out/"suite.jsonl",{"kind":"scale_complete","rows":size})
