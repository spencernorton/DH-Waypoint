#!/usr/bin/env python3
"""Replay saved page digests against an independent synthetic-population oracle."""
import argparse
from collections import Counter
from functools import lru_cache
import hashlib
import json
from pathlib import Path

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('inputs',nargs='+',type=Path)
p.add_argument('--out',required=True,type=Path)
args=p.parse_args()
rare={'identity_rare','external_correlated','external_identifier','external_keyset',
      'text_rare','raw_text_rare','raw_substring_rare'}
hot={'external_hot','external_hot_or_missing','external_hot_or_canonical','identity_episode_quality'}


@lru_cache(None)
def expected(size,case):
    accepted=case in {'identity_rare','external_correlated','external_identifier','external_keyset'} or case in hot
    candidates=[]
    if case in rare:
        first=((123456-1000)*pow(48271,-1,1000003))%1000003
        numbers=range(first or 1000003,size+1,1000003)
    else:
        numbers=range(20,size+1,20)
    for n in numbers:
        if case in rare and n%20==0:continue
        trusted=(n//20)%10<7
        if accepted and not trusted:continue
        if n%199==0 or n%211==0:continue
        if case=='identity_episode_quality' and not (n%8==0 and (n%11==0 or n%24==4)):continue
        h=hashlib.md5(('synthetic-'+str(n)).encode()).hexdigest()+hashlib.md5(('tail-'+str(n)).encode()).hexdigest()[:8]
        family=123456 if case in rare else 1
        identifier=h if case.startswith('raw_') else h+(':'+('movie' if family%3==0 else 'tv_show')+':synthetic:'+str(family) if trusted else ':?:?:?')
        candidates.append((n%100000,h,identifier))
    ordered=sorted(candidates,key=lambda r:r[1]) if case.startswith('raw_') else sorted(candidates,reverse=True)
    page=[(identifier,h) for _,h,identifier in ordered[:101 if case=='raw_substring_rare' else 100]]
    return hashlib.sha256(repr(page).encode()).hexdigest(),len(page)


checked=Counter();files=0
for root in args.inputs:
    for path in sorted(root.rglob('*.jsonl')):
        context={};used=False
        for line in path.open():
            row=json.loads(line)
            if row['kind']=='run_start':
                context={'rows':row['rows'],'updates':row['concurrent_updates']}
            if row['kind']!='sample':continue
            row={**context,**row}
            case=row['case']
            if row.get('updates') or case not in rare|hot:continue
            digest,count=expected(row['rows'],case)
            if (row['digest'],row['count'])!=(digest,count):
                raise SystemExit('saved page differs from complete independent oracle: '+path.name+' / '+case)
            checked[case]+=1;used=True
        files+=used
result={'format':1,'verified_sample_digests':sum(checked.values()),'files_with_verified_samples':files,
        'by_case':dict(sorted(checked.items())),'failures':0,
        'source_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
        'scope':'non-concurrent saved rare, hot-alias and combined-filter pages; independent modular population and hash/order oracle'}
args.out.write_text(json.dumps(result,indent=2,sort_keys=True)+'\n')
print(json.dumps(result,sort_keys=True))
