#!/usr/bin/env python3
"""Plot the separate raw-v2 predicate and prepared-identity experiments."""
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
rows=[r for r in data['latencies'] if r['rows']==32000000 and not r['concurrent_writes']]
colors={'candidate':'#53dfb0','selected':'#ffbd73'}
plt.rcParams.update({'font.family':'DejaVu Sans','font.size':10,'figure.facecolor':'#111722',
    'axes.facecolor':'#111722','axes.edgecolor':'#465267','text.color':'#edf1f7',
    'axes.labelcolor':'#d1dae8','xtick.color':'#b7c5d8','ytick.color':'#dbe4f1',
    'savefig.facecolor':'#111722'})
fig,axes=plt.subplots(1,2,figsize=(15,7))
profiles=[
 [('raw_substring_hot','literal','Substring · common'),('raw_substring_rare','literal','Substring · rare'),
  ('raw_substring_miss','literal','Substring · absent'),('raw_substring_wildcard','literal','Literal wildcard characters'),
  ('raw_v2_keyset','literal','Hash cursor · 101 rows')],
 [('identity_hot','auto','Hot canonical ID · automatic'),('identity_hot','force_generic_plan','Hot canonical ID · generic'),
  ('identity_episode_quality','auto','ID + episode + quality · automatic'),
  ('identity_episode_quality','force_generic_plan','ID + episode + quality · generic'),
  ('external_keyset','auto','External ID · automatic'),('external_keyset','force_generic_plan','External ID · generic')]
]
for ax,cases,title in zip(axes,profiles,['Raw v2 operator/order profile','Prepared identities · server plan mode']):
    for ci,(case,mode,label) in enumerate(cases):
        for vi,variant in enumerate(colors):
            y=ci+(vi-.5)*.25
            r=next((r for r in rows if r['case']==case and r['plan_mode']==mode and
                    r['variant']==variant and r['cache_state']=='warm'),None)
            if r:
                if r['errors']:
                    ax.scatter(5000,y,marker='>',s=45,color=colors[variant])
                elif r['samples']:
                    ax.plot([r['p50_ms'],r['p95_ms']],[y,y],color=colors[variant],lw=2)
                    ax.scatter([r['p50_ms'],r['p95_ms']],[y,y],s=[12,32],color=colors[variant])
            cold=next((r for r in rows if r['case']==case and r['plan_mode']==mode and
                       r['variant']==variant and r['cache_state']=='first_touch'),None)
            if cold:
                if cold['errors']:ax.scatter(5000,y+.07,marker='>',s=45,color=colors[variant])
                elif cold['samples']:ax.scatter(cold['p95_ms'],y+.07,s=55,facecolors='none',edgecolors=colors[variant],lw=1.5)
    ax.set_xscale('log');ax.set_xlim(.03,8500)
    ax.set_ylim(len(cases)-.5,-.6)
    ax.set_yticks(range(len(cases)),[v[2] for v in cases])
    ax.set_xticks([.1,1,10,100,1000,5000]);ax.xaxis.set_major_formatter(FuncFormatter(lambda x,pos:f'{x:g}'))
    ax.set_xlabel('SQL retrieval · ms · logarithmic scale',labelpad=12)
    ax.set_title(title,loc='left',fontsize=14,fontweight='bold',pad=18)
    ax.grid(axis='x',alpha=.18);ax.tick_params(axis='y',length=0,pad=10)
    ax.spines[['top','right','left']].set_visible(False)
legend=[Line2D([0],[0],color=color,marker='o',lw=2,label=variant.capitalize()) for variant,color in colors.items()]
legend.append(Line2D([0],[0],marker='o',markerfacecolor='none',color='#cdd6e3',lw=0,label='Cold p95 · selected cases'))
fig.legend(handles=legend,loc='upper right',bbox_to_anchor=(.99,.99),ncol=3,frameon=False,fontsize=9)
fig.suptitle('32 million torrents: predicate and plan sensitivity',x=.02,y=.99,ha='left',fontsize=18,fontweight='bold')
fig.text(.02,.04,'Warm dots span p50 → p95. Hollow dots are cold p95. Triangles mark timeouts at the 5-second budget; partial successes remain in JSON.\n'
    'Reduced serving model · 6-CPU / 16-GiB database cap · 30 warm observations scheduled / 10 selected cold trials · no HTTP latency claim.',
    fontsize=9,color='#b7c5d8')
fig.tight_layout(rect=(.01,.12,.995,.91),w_pad=3)
args.out.parent.mkdir(parents=True,exist_ok=True)
fig.savefig(args.out,dpi=180)
