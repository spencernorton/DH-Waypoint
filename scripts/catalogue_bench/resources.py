#!/usr/bin/env python3
"""Join sampled container counters to suite stages without inventing coverage."""
import argparse
import json
import math
from pathlib import Path
import statistics

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('summary',type=Path)
p.add_argument('resources',nargs='+',type=Path)
p.add_argument('--out',required=True,type=Path)
args=p.parse_args()
summary=json.loads(args.summary.read_text())
points=[]
for path in args.resources:
    with path.open() as source:
        for line in source:
            row=json.loads(line)
            row['file']=path.name
            points.append(row)
points.sort(key=lambda r:r['at'])

def bytes_value(text):
    value,unit=text.split()
    multiplier={'B':1,'KiB':2**10,'MiB':2**20,'GiB':2**30,'TiB':2**40,
                'kB':1000,'MB':1000**2,'GB':1000**3,'TB':1000**4}[unit]
    return float(value)*multiplier

def distribution(values):
    if not values:return None
    values=sorted(values)
    return {'samples':len(values),'min':values[0],'median':statistics.median(values),
            'p95':values[max(0,math.ceil(len(values)*.95)-1)],'max':values[-1]}

stages=[]
starts={}
for event in summary['stages']:
    if event['kind']=='stage_start':starts[event['stage']]=event['at']
    elif event['stage'] in starts:
        start=starts.pop(event['stage'])
        selected=[r for r in points if start<=r['at']<=event['at']]
        stages.append({'stage':event['stage'],'start':start,'end':event['at'],
            'exit_code':event['exit_code'],'sample_count':len(selected),
            'cpu_percent_one_core_is_100':distribution([float(r['CPUPerc'].rstrip('%')) for r in selected]),
            'docker_working_set_bytes':distribution([bytes_value(r['MemUsage'].split(' / ')[0]) for r in selected]),
            'cgroup_charged_bytes':distribution([r['memory.current'] for r in selected if 'memory.current' in r]),
            'cgroup_lifetime_peak_bytes':max((r['memory.peak'] for r in selected if 'memory.peak' in r),default=None)})
result={'format':1,'stages':stages,'resource_files':[p.name for p in args.resources],
        'limits':['Samples are approximately every seven seconds including docker stats latency.',
                  'Docker working set excludes inactive file cache; it is not total charged RAM.',
                  'Charged memory and lifetime peak include the container file cache.',
                  'Cgroup counters were added during the 10M run; missing earlier coverage stays null.',
                  'CPU percent uses 100 percent per core; the container cap is six cores.',
                  'A stage without samples has no measured resource distribution.']}
args.out.parent.mkdir(parents=True,exist_ok=True)
args.out.write_text(json.dumps(result,indent=2,sort_keys=True)+'\n')
print(json.dumps({'stages':len(stages),'with_samples':sum(r['sample_count']>0 for r in stages)}))
