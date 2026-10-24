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
args=p.parse_args()
raw_policy=bench.ELIGIBLE.replace('=tc.info_hash','=raw.info_hash')
identity=("SELECT tc.id,tc.info_hash FROM torrent_contents tc WHERE "
          "tc.content_type=$1 AND tc.content_source=$2 AND tc.content_id=$3 AND "+bench.ELIGIBLE+
          " ORDER BY "+bench.ORDER+" LIMIT 100")
external=("SELECT tc.id,tc.info_hash FROM torrent_contents tc WHERE "
          "(tc.content_type,tc.content_source,tc.content_id) IN (VALUES ($1,$2,$3) "
          "UNION ALL SELECT content_type,content_source,content_id FROM content_attributes "
          "WHERE content_type=$1 AND source=$2 AND value=$3 AND key='id') AND "+bench.ELIGIBLE+
          " ORDER BY "+bench.ORDER+" LIMIT 100")
raw=("SELECT encode(raw.info_hash,'hex') AS id,raw.info_hash FROM torrents raw "
     "WHERE raw.tsv @@ $1::tsquery AND "+raw_policy+" ORDER BY raw.info_hash LIMIT 100")
substring=("SELECT encode(raw.info_hash,'hex') AS id,raw.info_hash FROM torrents raw "
           "WHERE raw.name ILIKE $1 AND "+raw_policy+" ORDER BY raw.info_hash LIMIT 101")
cases={
    "identity_hot":(identity,["tv_show","synthetic","1"]),
    "identity_rare":(identity,["movie","synthetic","123456"]),
    "external_keyset":(external,["movie","imdb","tt123456"]),
    "raw_text_hot":(raw,["title1"]),
    "raw_text_rare":(raw,["title123456"]),
    "raw_text_miss":(raw,["unfindabletoken"]),
    "raw_substring_hot":(substring,["%title1%"]),
    "raw_substring_rare":(substring,["%title123456 %"]),
    "raw_substring_miss":(substring,["%unfindabletoken%"]),
}
bench.emit(args.out,{"kind":"prepared_manifest","source_sha256":hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                    "bench_sha256":hashlib.sha256(Path(bench.__file__).read_bytes()).hexdigest(),
                    "variant":args.variant,"samples":args.samples})
for mode in ["force_custom_plan","force_generic_plan"]:
    for name,(query,params) in cases.items():
        with bench.connect(os.environ['CATALOGUE_BENCH_DSN']) as conn:
            conn.execute("SET statement_timeout=180000")
            rows=conn.execute("SELECT count(*) FROM torrents").fetchone()[0]
            conn.execute("SET statement_timeout=5000")
            conn.execute("SET plan_cache_mode="+mode)
            conn.execute("PREPARE catalogue_probe ("+','.join(['text']*len(params))+") AS "+query)
            execute=sql.SQL("EXECUTE catalogue_probe ({})").format(sql.SQL(',').join(sql.Literal(x) for x in params))
            bench.emit(args.out,{"kind":"prepared_query","case":name,"plan_mode":mode,"sql":query,"params":params})
            try:
                result=conn.execute(execute,prepare=False).fetchall()
                bench.verify_page(conn,args,name,result)
                for i in range(args.samples):
                    start=time.perf_counter()
                    result=conn.execute(execute,prepare=False).fetchall()
                    bench.emit(args.out,{"kind":"sample","rows":rows,"variant":args.variant,"cache_state":"warm",
                      "updates":False,"case":name,"sample":i,"ms":(time.perf_counter()-start)*1000,
                      "count":len(result),"digest":bench.digest(result),"plan_mode":mode})
                plan=conn.execute(sql.SQL("EXPLAIN (ANALYZE,BUFFERS,WAL,SETTINGS,FORMAT JSON) ")+execute).fetchone()[0]
                bench.emit(args.out,{"kind":"plan","rows":rows,"variant":args.variant,"case":name,"plan_mode":mode,"plan":plan})
            except psycopg.Error as err:
                bench.emit(args.out,{"kind":"sample_error","rows":rows,"variant":args.variant,"case":name,
                           "cache_state":"warm","updates":False,"plan_mode":mode,"sqlstate":err.sqlstate})
