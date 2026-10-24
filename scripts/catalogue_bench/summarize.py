#!/usr/bin/env python3
"""Summarize retained JSONL without discarding failed or skipped observations."""
import argparse
from collections import defaultdict
import gzip
import hashlib
import json
import math
from pathlib import Path

p=argparse.ArgumentParser(description=__doc__)
p.add_argument("inputs",nargs="+",type=Path)
p.add_argument("--out",required=True,type=Path)
args=p.parse_args()
files=[]
labels={}
for item in args.inputs:
    selected=sorted([*item.rglob('*.jsonl'),*item.rglob('*.jsonl.gz')]) if item.is_dir() else [item]
    files.extend(selected)
    for file in selected:
        labels[file]=str(Path(item.name)/file.relative_to(item)) if item.is_dir() else str(Path(item.parent.name)/item.name)
if not files:
    p.error("no raw JSONL files found")
if len(set(labels.values()))!=len(files):
    p.error("duplicate raw paths; select distinct qualified run folders")
groups=defaultdict(lambda:{"samples":[],"errors":0,"warmup_errors":0,"skipped":0,
                          "sqlstates":defaultdict(int),"counts":set(),"files":set()})
storage=[]
writes=[]
indexes=[]
reindex=[]
recovery=[]
correctness=[]
manifests=[]
complete=set()
inventory=[]
stages=[]
prepared=[]
plan_errors=[]


def key(row):
    return (row.get('rows'),row.get('variant'),row.get('cache_state'),row.get('updates',False),
            row.get('case'),row.get('plan_mode','literal'))


for file in files:
    label=labels[file]
    inventory.append({"name":label,"sha256":hashlib.sha256(file.read_bytes()).hexdigest(),"bytes":file.stat().st_size})
    context={}
    prepared_variant=None
    reader=gzip.open if file.suffix=='.gz' else open
    with reader(file,'rt') as source:
        for line in source:
            row=json.loads(line)
            kind=row['kind']
            if kind=='run_start':
                context={k:row[k] for k in ['rows','variant','cache_state']}
                context['updates']=row['concurrent_updates']
            if kind in ['sample','sample_error','sample_skipped','warmup_error']:
                data={**context,**row}
                group=groups[key(data)]
                group['files'].add(label)
                if kind=='sample':
                    group['samples'].append(row['ms']);group['counts'].add(row['count'])
                elif kind=='sample_skipped':group['skipped']+=1
                else:
                    group['errors']+=1
                    group['warmup_errors']+=kind=='warmup_error'
                    group['sqlstates'][str(row.get('sqlstate'))]+=1
            elif kind=='storage':storage.append({"file":label,**row})
            elif kind=='update_summary' and row.get('batches'):
                batches=row['batches']
                seconds=row['seconds']
                writes.append({"file":label,"rows":row['rows'],"variant":row['variant'],"seconds":seconds,
                  "updated_torrents":row['updated_rows'],"inserted_torrents":row.get('inserted_rows',0),
                  "updates_per_second":row['updated_rows']/seconds,
                  "inserts_per_second":row.get('inserted_rows',0)/seconds,
                  "batch_errors":sum('error' in b for b in batches)})
            elif kind in ['index_change','index_build']:indexes.append({"file":label,**row})
            elif kind=='reindex':reindex.append({"file":label,**row})
            elif kind=='crash_recovery':recovery.append(row)
            elif kind in ['correctness','page_correctness','authority_check','raw_without_projection']:
                correctness.append({"file":label,**row})
            elif kind in ['manifest','prepared_manifest','supplement_manifest']:
                manifests.append(row)
                if kind=='prepared_manifest':prepared_variant=row['variant']
            elif kind=='prepared_counters':prepared.append({'file':label,'variant':prepared_variant,**row})
            elif kind=='plan_error':plan_errors.append({'file':label,
                **{k:context[k] for k in ['rows','variant'] if k in context},**row})
            elif kind=='scale_complete':complete.add(row['rows'])
            elif kind in ['stage_start','stage_end']:stages.append({'file':label,**row})


def quantile(values,q):
    return values[max(0,math.ceil(q*len(values))-1)] if values else None


latencies=[]
for k,group in groups.items():
    values=sorted(group['samples'])
    latencies.append({"rows":k[0],"variant":k[1],"cache_state":k[2],"concurrent_writes":k[3],"case":k[4],"plan_mode":k[5],
      "samples":len(values),"errors":group['errors'],"skipped":group['skipped'],"p50_ms":quantile(values,.5),
      "warmup_errors":group['warmup_errors'],"sqlstates":dict(group['sqlstates']),
      "p95_ms":quantile(values,.95),"p99_ms":quantile(values,.99),"min_ms":min(values) if values else None,
      "max_ms":max(values) if values else None,"result_counts":sorted(group['counts']),"files":sorted(group['files'])})
result={"format":1,"quantile_method":"nearest rank; successes only; failures/skips retained separately",
  "completed_scales":sorted(complete),"manifests":manifests,"latencies":latencies,
  "storage":storage,"concurrent_writes":writes,"index_operations":indexes,"reindex":reindex,"recovery":recovery,
  "correctness":correctness,"stages":stages,"raw_files":inventory,
  "prepared_plan_counters":prepared,
  "plan_errors":plan_errors,
  "limits":["SQL microbenchmark; no HTTP serialization or complete production schema",
            "20-byte synthetic hashes; stated skew and file distribution",
            "30 warm and 10 selected cold samples give limited p99 resolution",
            "cold evicts only stopped database files; device caches remain uncontrolled",
            "timeout groups must not be described using successful latency quantiles alone"]}
args.out.parent.mkdir(parents=True,exist_ok=True)
args.out.write_text(json.dumps(result,indent=2,sort_keys=True)+'\n')
print(json.dumps({"completed_scales":result['completed_scales'],"latency_groups":len(latencies),
                  "groups_with_errors":sum(x['errors']>0 for x in latencies),"raw_files":len(inventory)}))
