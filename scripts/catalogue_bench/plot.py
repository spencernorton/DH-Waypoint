#!/usr/bin/env python3
"""Plot measured warm SQL percentiles from summarize.py (requires matplotlib)."""
import argparse
import json
from pathlib import Path

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
from matplotlib.lines import Line2D
from matplotlib.ticker import FuncFormatter

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('summary',type=Path)
p.add_argument('--out',type=Path,required=True)
args=p.parse_args()
data=json.loads(args.summary.read_text())
groups=[r for r in data['latencies'] if r['cache_state']=='warm' and not r['concurrent_writes'] and r['plan_mode']=='literal']
sizes=sorted(set(r['rows'] for r in groups if r['rows']))
cases=[('raw_text_rare','Raw name · rare'),('external_keyset','External ID'),
       ('identity_hot','Canonical ID · hot'),('type_browse','Type browse'),
       ('offset','Offset · 1,000,000'),('keyset','Published cursor'),
       ('enriched_with_fallback','Full fallback union'),('enriched_bounded_fallback','Bounded fallback merge')]
variants=['baseline','candidate','selected']
colors={'baseline':'#8eacff','candidate':'#53dfb0','selected':'#ffbd73'}
plt.rcParams.update({'font.family':'DejaVu Sans','font.size':10,'figure.facecolor':'#111722',
                    'axes.facecolor':'#111722','axes.edgecolor':'#465267','text.color':'#edf1f7',
                    'axes.labelcolor':'#d1dae8','xtick.color':'#b7c5d8','ytick.color':'#dbe4f1',
                    'savefig.facecolor':'#111722'})
fig,axes=plt.subplots(1,len(sizes),figsize=(max(7,5.4*len(sizes)),7.2),squeeze=False,sharey=True)
for ax,size in zip(axes[0],sizes):
    for ci,(case,label) in enumerate(cases):
        for vi,variant in enumerate(variants):
            row=next((r for r in groups if r['rows']==size and r['case']==case and r['variant']==variant),None)
            if row is None:continue
            y=ci+(vi-1)*.18
            if row['errors']:
                ax.scatter(5000,y,color=colors[variant],marker='>',s=45,zorder=3)
                continue
            if not row['samples']:continue
            ax.plot([row['p50_ms'],row['p95_ms']],[y,y],color=colors[variant],lw=2,alpha=.8)
            ax.scatter(row['p50_ms'],y,color=colors[variant],s=13,zorder=3)
            ax.scatter(row['p95_ms'],y,color=colors[variant],s=36,zorder=3,edgecolor='#111722',lw=.5)
    ax.set_xscale('log')
    ax.set_xlim(.03,8500)
    ax.set_ylim(len(cases)-.5,-.6)
    ax.set_yticks(range(len(cases)),[label for _,label in cases])
    ax.set_xticks([.1,1,10,100,1000,5000])
    ax.xaxis.set_major_formatter(FuncFormatter(lambda x,pos:f'{x:g}'))
    ax.grid(axis='x',which='major',alpha=.18)
    ax.set_xlabel('SQL latency · ms · logarithmic scale',labelpad=12)
    ax.set_title(f'{size/1e6:g} million torrents',loc='left',fontsize=15,fontweight='bold',pad=18)
    ax.spines[['top','right','left']].set_visible(False)
    ax.tick_params(axis='y',length=0,pad=10)
legend=[Line2D([0],[0],color=colors[v],marker='o',lw=2,label=v.capitalize()) for v in variants if any(r['variant']==v for r in groups)]
fig.legend(handles=legend,loc='upper right',bbox_to_anchor=(.99,.99),ncol=len(legend),frameon=False)
fig.suptitle('Catalogue retrieval: measured p50 → p95',x=.02,y=.99,ha='left',fontsize=18,fontweight='bold')
fig.text(.02,.045,'Dots start at p50 and end at p95. Triangles mark query errors at the 5-second budget.\n'
         'Synthetic SQL workload · PostgreSQL 16.15 · 6-CPU / 16-GiB cap · 30 scheduled warm observations · no HTTP latency claim.',
         fontsize=9,color='#b7c5d8',va='bottom')
fig.tight_layout(rect=(.01,.12,.995,.92),w_pad=2)
args.out.parent.mkdir(parents=True,exist_ok=True)
fig.savefig(args.out,dpi=180)
