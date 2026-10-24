#!/usr/bin/env python3
"""Compare parameter-sensitive custom/generic plans on the same fixture."""
import argparse
import hashlib
import os
from pathlib import Path
import time

from psycopg import sql
import psycopg

import bench

p=argparse.ArgumentParser(description=__doc__)
p.add_argument("--out",required=True)
p.add_argument("--variant",required=True)
p.add_argument("--samples",type=int,default=30)
p.add_argument('--cases',help='optional comma-separated prepared cases')
args=p.parse_args()
raw_policy=bench.ELIGIBLE.replace('=tc.info_hash','=raw.info_hash')
identity=("SELECT tc.id,tc.info_hash FROM torrent_contents tc WHERE "
          "tc.content_type=$1 AND tc.content_source=$2 AND tc.content_id=$3 AND "+bench.ELIGIBLE+
          " ORDER BY "+bench.ORDER+" LIMIT 100")
filtered_identity=identity.replace(' AND '+bench.ELIGIBLE,
    " AND tc.video_resolution='2160p' AND (tc.episodes #> '{0}'='{}'::jsonb OR "
    "tc.episodes #> '{0}' @> '{\"4\":{}}'::jsonb) AND "+bench.ELIGIBLE)
external=("SELECT tc.id,tc.info_hash FROM torrent_contents tc WHERE "
          "(tc.content_type,tc.content_source,tc.content_id) IN (VALUES ($1,$2,$3) "
          "UNION ALL SELECT content_type,content_source,content_id FROM content_attributes "
          "WHERE content_type=$1 AND source=$2 AND value=$3 AND key='id') AND "+bench.ELIGIBLE+
          " ORDER BY "+bench.ORDER+" LIMIT 100")
raw=("SELECT encode(raw.info_hash,'hex') AS id,raw.info_hash FROM torrents raw "
     "WHERE raw.tsv @@ $1::tsquery AND "+raw_policy+" ORDER BY raw.info_hash LIMIT 100")
substring=("SELECT encode(raw.info_hash,'hex') AS id,raw.info_hash FROM torrents raw "
           "WHERE raw.name ILIKE $1 AND "+bench.raw_serving_policy()+" ORDER BY raw.info_hash LIMIT 101")
raw_cursor=("SELECT encode(raw.info_hash,'hex') AS id,raw.info_hash FROM torrents raw "
            "WHERE raw.info_hash>decode($1,'hex') AND "+bench.raw_serving_policy()+" ORDER BY raw.info_hash LIMIT 101")
cases={
    "identity_hot":(identity,["tv_show","synthetic","1"]),
    "identity_rare":(identity,["movie","synthetic","123456"]),
    "identity_episode_quality":(filtered_identity,["tv_show","synthetic","1"]),
    "external_keyset":(external,["movie","imdb","tt123456"]),
    "external_hot":(external,["tv_show","imdb","tt1"]),
    "raw_text_hot":(raw,["title1"]),
    "raw_text_rare":(raw,["title123456"]),
    "raw_text_miss":(raw,["unfindabletoken"]),
    "raw_substring_hot":(substring,["%title1%"]),
    "raw_substring_rare":(substring,["%title123456 %"]),
    "raw_substring_miss":(substring,["%unfindabletoken%"]),
    "raw_v2_keyset":(raw_cursor,["80"+"00"*19]),
}
if args.cases:
    wanted=set(args.cases.split(','))
    if wanted-cases.keys():p.error('unknown prepared case')
    cases={k:v for k,v in cases.items() if k in wanted}
with bench.connect(os.environ['CATALOGUE_BENCH_DSN']) as metadata:
    metadata.execute('SET statement_timeout=180000')
    rows=metadata.execute('SELECT count(*) FROM torrents').fetchone()[0]
bench.emit(args.out,{"kind":"prepared_manifest","rows":rows,"source_sha256":hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                    "bench_sha256":hashlib.sha256(Path(bench.__file__).read_bytes()).hexdigest(),
                    "variant":args.variant,"samples":args.samples})
for mode in ["auto","force_custom_plan","force_generic_plan"]:
    for name,(query,params) in cases.items():
        with bench.connect(os.environ['CATALOGUE_BENCH_DSN']) as conn:
            conn.execute("SET statement_timeout=5000")
            conn.execute("SET plan_cache_mode="+mode)
            conn.execute("PREPARE catalogue_probe ("+','.join(['text']*len(params))+") AS "+query)
            execute=sql.SQL("EXECUTE catalogue_probe ({})").format(sql.SQL(',').join(sql.Literal(x) for x in params))
            bench.emit(args.out,{"kind":"prepared_query","case":name,"plan_mode":mode,"sql":query,"params":params})
            phase='warmup'
            attempted=0
            try:
                result=conn.execute(execute,prepare=False).fetchall()
                bench.verify_page(conn,args,name,result)
                for i in range(args.samples):
                    phase='sample'
                    attempted=i+1
                    start=time.perf_counter()
                    result=conn.execute(execute,prepare=False).fetchall()
                    bench.emit(args.out,{"kind":"sample","rows":rows,"variant":args.variant,"cache_state":"warm",
                      "updates":False,"case":name,"sample":i,"ms":(time.perf_counter()-start)*1000,
                      "count":len(result),"digest":bench.digest(result),"plan_mode":mode})
                phase='plan'
                plan=conn.execute(sql.SQL("EXPLAIN (ANALYZE,BUFFERS,WAL,SETTINGS,FORMAT JSON) ")+execute).fetchone()[0]
                bench.emit(args.out,{"kind":"plan","rows":rows,"variant":args.variant,"case":name,"plan_mode":mode,"plan":plan})
            except psycopg.Error as err:
                kind={'warmup':'warmup_error','sample':'sample_error','plan':'plan_error'}[phase]
                bench.emit(args.out,{"kind":kind,"rows":rows,"variant":args.variant,"case":name,
                           "cache_state":"warm","updates":False,"plan_mode":mode,"sqlstate":err.sqlstate,
                           'phase':phase,'sample':attempted-1 if phase=='sample' else None})
                if err.sqlstate!='57014':raise
                for i in range(attempted,args.samples):
                    bench.emit(args.out,{'kind':'sample_skipped','rows':rows,'variant':args.variant,'case':name,
                        'cache_state':'warm','updates':False,'plan_mode':mode,'sample':i,'reason':'prior timeout'})
            counters=conn.execute("SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name='catalogue_probe'").fetchone()
            bench.emit(args.out,{'kind':'prepared_counters','case':name,'plan_mode':mode,
                       'rows':rows,'variant':args.variant,'generic_plans':counters[0],'custom_plans':counters[1]})
