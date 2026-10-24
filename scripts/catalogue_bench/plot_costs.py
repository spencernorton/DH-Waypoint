#!/usr/bin/env python3
"""Plot storage and observed mixed-write costs; never extrapolate capacity."""
import argparse
import json
from pathlib import Path

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('summary',type=Path)
p.add_argument('--out',type=Path,required=True)
args=p.parse_args()
data=json.loads(args.summary.read_text())
sizes=data['completed_scales']
plt.rcParams.update({'font.family':'DejaVu Sans','font.size':10,'figure.facecolor':'#111722',
    'axes.facecolor':'#111722','axes.edgecolor':'#465267','text.color':'#edf1f7',
    'axes.labelcolor':'#d1dae8','xtick.color':'#b7c5d8','ytick.color':'#dbe4f1',
    'savefig.facecolor':'#111722'})
fig,axes=plt.subplots(2,len(sizes),figsize=(max(7,5*len(sizes)),8),squeeze=False)
for col,size in enumerate(sizes):
    storage={}
    for row in sorted(data['storage'],key=lambda r:r['at']):
        if row['rows']!=size or not row['file'].endswith('-indexes.jsonl'):continue
        variant=row['file'].rsplit('/',1)[-1].split('-')[1]
        if variant not in ['baseline','candidate','retained']:continue
        total=sum(r[3] for r in row['relations'])/2**30
        indexes=sum(r[2] for r in row['relations'])/2**30
        storage[variant]=(total-indexes,indexes)
    variants=[v for v in ['baseline','candidate','retained'] if v in storage]
    x=list(range(len(variants)))
    ax=axes[0,col]
    table=[storage[v][0] for v in variants];indexes=[storage[v][1] for v in variants]
    ax.bar(x,table,color='#8eacff',label='Table storage incl. TOAST/FSM/VM')
    ax.bar(x,indexes,bottom=table,color='#53dfb0',label='Indexes')
    for i,(t,ix) in enumerate(zip(table,indexes)):
        ax.text(i,t+ix,f'{t+ix:.2f}',ha='center',va='bottom',fontsize=10)
    ax.set_ylim(0,max((a+b for a,b in zip(table,indexes)),default=1)*1.18)
    ax.set_title(f'{size/1e6:g} million torrents',loc='left',fontsize=15,fontweight='bold',pad=16)
    ax.set_ylabel('Reduced-schema total · GiB')
    ax.set_xticks(x,[v.capitalize() for v in variants])
    ax.grid(axis='y',alpha=.15)
    ax=axes[1,col]
    writes={r['variant']:r for r in data['concurrent_writes'] if r['rows']==size and '-substring-' not in r['file'] and not r['file'].endswith('/raw-concurrent.jsonl')}
    ax.bar([i-.18 for i in x],[writes[v]['inserts_per_second'] for v in variants],width=.34,color='#53dfb0',label='Inserted torrents/s')
    ax.bar([i+.18 for i in x],[writes[v]['updates_per_second'] for v in variants],width=.34,color='#8eacff',label='Reobserved torrents/s')
    ax.set_xticks(x,[v.capitalize() for v in variants])
    ax.set_ylabel('Observed records per second')
    ax.set_ylim(0,max((max(writes[v]['inserts_per_second'],writes[v]['updates_per_second']) for v in variants),default=1)*1.3)
    ax.grid(axis='y',alpha=.15)
    for a in axes[:,col]:a.spines[['top','right']].set_visible(False)
axes[0,0].legend(loc='upper left',frameon=False,fontsize=8)
axes[1,0].legend(loc='upper left',frameon=False,fontsize=8)
fig.suptitle('Index costs: storage and mixed-write observations',x=.02,y=.985,ha='left',fontsize=18,fontweight='bold')
fig.text(.02,.035,'Same 25 SQL cases per main profile; each index set has one measured writer window. Query timeouts remain in the raw evidence.\n'
    'Baseline/candidate include a historical raw-TSV extension absent from the current backend; migration72 adds only two identity indexes.\n'
    'Synthetic insert/reobservation transactions and a reduced schema; these rates are not a production capacity forecast.',fontsize=9,color='#b7c5d8')
fig.tight_layout(rect=(.01,.13,.995,.94),h_pad=3,w_pad=3)
args.out.parent.mkdir(parents=True,exist_ok=True)
fig.savefig(args.out,dpi=180)
