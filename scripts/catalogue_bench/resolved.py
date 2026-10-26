#!/usr/bin/env python3
"""Measure snapshot alias resolution plus its ordered page, including round trips."""
import argparse
import hashlib
import os
from pathlib import Path
import time

import psycopg
from psycopg import sql

import bench

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--out',required=True)
p.add_argument('--samples',type=int,default=30)
args=p.parse_args()
args.updates=False
rows=32000000
cases={
    'external_hot':[('tv_show','imdb','tt1')],
    'external_keyset':[('movie','imdb','tt123456')],
    'external_hot_or_missing':[('tv_show','imdb','tt1'),('tv_show','tvdb','missing')],
    'external_hot_or_canonical':[('tv_show','imdb','tt1'),('tv_show','synthetic','1')],
    'external_two_keys':[('tv_show','imdb','tt1'),('movie','imdb','tt123456')],
}
with bench.connect(os.environ['CATALOGUE_BENCH_DSN']) as metadata:
    metadata.execute('SET statement_timeout=180000')
    assert metadata.execute('SELECT count(*) FROM torrents').fetchone()[0]==rows
bench.emit(args.out,{'kind':'prepared_manifest','rows':rows,'variant':'resolved','samples':args.samples,
    'source_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
    'bench_sha256':hashlib.sha256(Path(bench.__file__).read_bytes()).hexdigest(),
    'timing':'BEGIN read-only repeatable-read + lookup + page + COMMIT; no hydration or HTTP'})


MAX_RESOLVED_IDENTIFIER_KEYS=32


def execution(name,values):
    return sql.SQL('EXECUTE {} ({})').format(sql.Identifier(name),sql.SQL(',').join(sql.Literal(v) for v in values))


def resolved_page_sql(nkeys):
    arms=['(tc.content_type = $%d AND tc.content_source = $%d AND tc.content_id = $%d)'%(i*3+1,i*3+2,i*3+3) for i in range(nkeys)]
    predicate=arms[0] if nkeys==1 else '('+' OR '.join(arms)+')'
    return 'SELECT tc.id,tc.info_hash FROM torrent_contents tc WHERE '+predicate+' AND '+bench.ELIGIBLE+' ORDER BY '+bench.ORDER+' LIMIT 100'


for mode in ['auto','force_custom_plan','force_generic_plan']:
    for name,refs in cases.items():
        with bench.connect(os.environ['CATALOGUE_BENCH_DSN']) as conn:
            conn.execute('SET plan_cache_mode='+mode)
            values=','.join('('+','.join('$'+str(i*3+j+1) for j in range(3))+')' for i in range(len(refs)))
            alternatives=' OR '.join('(content_type=$%d AND source=$%d AND value=$%d)'%(i*3+1,i*3+2,i*3+3) for i in range(len(refs)))
            keys='VALUES '+values+' UNION ALL SELECT content_type,content_source,content_id FROM content_attributes WHERE key=\'id\' AND ('+alternatives+')'
            lookup='SELECT DISTINCT content_type,content_source,content_id FROM (SELECT type AS content_type,source AS content_source,id AS content_id FROM content WHERE (type,source,id) IN (VALUES '+values+') UNION ALL SELECT content_type,content_source,content_id FROM content_attributes WHERE key=\'id\' AND ('+alternatives+')) identifier_keys LIMIT 33'
            guard='(tc.content_type,tc.content_source,tc.content_id) IN ('+keys+')'
            page='SELECT tc.id,tc.info_hash FROM torrent_contents tc WHERE '+guard+' AND '+bench.ELIGIBLE+' ORDER BY '+bench.ORDER+' LIMIT 100'
            empty_page='SELECT tc.id,tc.info_hash FROM torrent_contents tc WHERE false AND '+bench.ELIGIBLE+' ORDER BY '+bench.ORDER+' LIMIT 100'
            params=[v for ref in refs for v in ref]
            for statement,query,nparams in [('resolve',lookup,len(params)),('page',page,len(params))]:
                conn.execute('PREPARE '+statement+' ('+','.join(['text']*nparams)+') AS '+query)
            bench.emit(args.out,{'kind':'prepared_query','case':name,'plan_mode':mode,'lookup_sql':lookup,'page_sql':page,'resolved_sql':resolved_page_sql(1),'params':params})
            resolved_stmts={}

            def run():
                start=time.perf_counter()
                conn.execute('BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY')
                try:
                    keys=sorted(conn.execute(execution('resolve',params),prepare=False).fetchall())
                    if len(keys)==0:
                        result=[]
                        query=sql.SQL(empty_page)
                    elif len(keys)>MAX_RESOLVED_IDENTIFIER_KEYS:
                        query=execution('page',params)
                        result=conn.execute(query,prepare=False).fetchall()
                    else:
                        statement='resolved_page_%d'%len(keys)
                        if statement not in resolved_stmts:
                            conn.execute('PREPARE '+statement+' ('+','.join(['text']*(len(keys)*3))+') AS '+resolved_page_sql(len(keys)))
                            resolved_stmts[statement]=True
                        query=execution(statement,[v for key in keys for v in key])
                        result=conn.execute(query,prepare=False).fetchall()
                    conn.execute('COMMIT')
                except BaseException:
                    conn.execute('ROLLBACK')
                    raise
                return (time.perf_counter()-start)*1000,result,len(keys),query

            phase='warmup';attempted=0
            try:
                _,result,key_count,executed=run()
                oracle='external_hot' if name.startswith('external_hot') else name
                bench.verify_page(conn,args,oracle,result)
                for i in range(args.samples):
                    phase='sample';attempted=i+1
                    ms,result,key_count,executed=run()
                    bench.emit(args.out,{'kind':'sample','rows':rows,'variant':'resolved','cache_state':'warm','updates':False,
                        'case':name,'plan_mode':mode,'sample':i,'ms':ms,'count':len(result),'digest':bench.digest(result),'key_count':key_count})
                phase='plan'
                plan=conn.execute(sql.SQL('EXPLAIN (ANALYZE,BUFFERS,WAL,SETTINGS,FORMAT JSON) ')+executed).fetchone()[0]
                bench.emit(args.out,{'kind':'plan','rows':rows,'variant':'resolved','case':name,'plan_mode':mode,'plan':plan})
            except psycopg.Error as err:
                bench.emit(args.out,{'kind':{'warmup':'warmup_error','sample':'sample_error','plan':'plan_error'}[phase],
                    'rows':rows,'variant':'resolved','cache_state':'warm','updates':False,'case':name,'plan_mode':mode,
                    'sqlstate':err.sqlstate,'phase':phase,'sample':attempted-1 if phase=='sample' else None})
                if err.sqlstate!='57014':raise
                for i in range(attempted,args.samples):
                    bench.emit(args.out,{'kind':'sample_skipped','rows':rows,'variant':'resolved','cache_state':'warm',
                        'updates':False,'case':name,'plan_mode':mode,'sample':i,'reason':'prior timeout'})
            for statement,generic,custom in conn.execute('SELECT name,generic_plans,custom_plans FROM pg_prepared_statements'):
                bench.emit(args.out,{'kind':'prepared_counters','rows':rows,'variant':'resolved','case':name,'plan_mode':mode,
                    'statement':statement,'generic_plans':generic,'custom_plans':custom})
