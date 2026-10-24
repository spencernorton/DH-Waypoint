#!/usr/bin/env python3
"""Disposable SQL-plan benchmark. Never connects to the application database.

Install requirements.txt in a venv. See README.md for the complete protocol.
The database must be named dh_waypoint_bench_*. Every raw observation is JSONL.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import random
import shutil
import threading
import time

import psycopg

SCHEMA = "catalogue_bench"
IDENTITY = "CASE WHEN n % 20 = 0 THEN 1 ELSE 1000 + ((n * 48271) % 1000003) END"
TRUSTED = "((n / 20) % 10 < 7)"
TYPE = "CASE WHEN family % 3 = 0 THEN 'movie' ELSE 'tv_show' END"
ORDER = "published_at DESC, info_hash DESC, id DESC"

DDL = """
CREATE SCHEMA catalogue_bench;
SET search_path = catalogue_bench;
CREATE TABLE sentinel (format text PRIMARY KEY);
INSERT INTO sentinel VALUES ('synthetic-catalogue-v1');
CREATE TABLE torrents (
 n bigint UNIQUE NOT NULL, info_hash bytea PRIMARY KEY, name text NOT NULL,
 size bigint NOT NULL, private boolean NOT NULL, files_count int NOT NULL,
 tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple',name)) STORED,
 created_at timestamptz NOT NULL
);
CREATE TABLE content (
 type text NOT NULL, source text NOT NULL, id text NOT NULL, title text NOT NULL,
 adult boolean NOT NULL, PRIMARY KEY(type,source,id)
);
CREATE TABLE content_attributes (
 content_type text NOT NULL, content_source text NOT NULL, content_id text NOT NULL,
 source text NOT NULL, key text NOT NULL, value text NOT NULL,
 PRIMARY KEY(content_type,content_source,content_id,source,key)
);
CREATE TABLE torrent_contents (
 id text GENERATED ALWAYS AS (encode(info_hash,'hex') || ':' ||
   coalesce(content_type,'?') || ':' || coalesce(content_source,'?') || ':' ||
   coalesce(content_id,'?')) STORED PRIMARY KEY,
 info_hash bytea NOT NULL REFERENCES torrents(info_hash),
 content_type text, content_source text, content_id text,
 search_string text NOT NULL,
 tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple',search_string)) STORED,
 episodes jsonb, video_resolution text, seeders bigint,
 published_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 FOREIGN KEY(content_type,content_source,content_id) REFERENCES content(type,source,id)
);
CREATE TABLE torrent_files (
 info_hash bytea NOT NULL REFERENCES torrents(info_hash), "index" int NOT NULL,
 path text NOT NULL, size bigint NOT NULL, PRIMARY KEY(info_hash,"index")
);
CREATE TABLE annotations (
 info_hash bytea PRIMARY KEY REFERENCES torrents(info_hash), plugin_id text NOT NULL,
 source_revision int NOT NULL, current_revision int NOT NULL,
 confidence real NOT NULL, verification text NOT NULL,
 proposed_type text NOT NULL, proposed_id text NOT NULL
);
CREATE TABLE quarantined (info_hash bytea PRIMARY KEY REFERENCES torrents(info_hash));
"""

# These model existing serving-query indexes, not every historical schema index.
BASE_INDEXES = [
 "CREATE INDEX raw_tsv_btree ON torrents(tsv)",
 "CREATE INDEX tc_hash ON torrent_contents(info_hash)",
 "CREATE INDEX tc_type ON torrent_contents(content_type)",
 "CREATE INDEX tc_source_id ON torrent_contents(content_source,content_id)",
 "CREATE INDEX tc_published ON torrent_contents(published_at)",
 "CREATE INDEX tc_tsv ON torrent_contents USING gin(tsv)",
 "CREATE INDEX tc_episodes_btree ON torrent_contents(episodes)",
 "CREATE INDEX tc_group ON torrent_contents(content_type,content_source,content_id,"
 "(coalesce(seeders,-1)) DESC,info_hash) WHERE content_id IS NOT NULL",
]
CANDIDATE_INDEXES = [
 "CREATE INDEX raw_tsv_gin ON torrents USING gin(tsv)",
 "CREATE INDEX tc_type_page ON torrent_contents(content_type,published_at DESC,info_hash DESC,id DESC)",
 "CREATE INDEX tc_page ON torrent_contents(published_at DESC,info_hash DESC,id DESC)",
 "CREATE INDEX tc_episodes_gin ON torrent_contents USING gin(episodes)",
 "CREATE INDEX ca_external ON content_attributes(source,value,content_type,content_source,content_id) WHERE key='id'",
]
IDENTITY_PAGE_INDEX = """CREATE INDEX tc_identity_page ON torrent_contents(
 content_type,content_source,content_id,published_at DESC,info_hash DESC,id DESC)
 WHERE content_id IS NOT NULL"""
RAW_SUBSTRING_INDEX = "CREATE INDEX raw_name_trgm ON torrents USING gin(name public.gin_trgm_ops)"

# Current catalogue authority is read separately from attributed plugin proposals.
ELIGIBLE = """EXISTS (SELECT 1 FROM torrents t WHERE t.info_hash=tc.info_hash AND NOT t.private)
 AND NOT EXISTS (SELECT 1 FROM quarantined q WHERE q.info_hash=tc.info_hash)
 AND NOT EXISTS (SELECT 1 FROM torrent_contents serving_tc LEFT JOIN content c
 ON (c.type,c.source,c.id)=(serving_tc.content_type,serving_tc.content_source,serving_tc.content_id)
 WHERE serving_tc.info_hash=tc.info_hash AND (serving_tc.content_type='xxx' OR c.adult IS TRUE))"""


def emit(path, obj):
    obj = {"at": time.time(), **obj}
    with Path(path).open("a") as f:
        f.write(json.dumps(obj, sort_keys=True, default=str) + "\n")


def connect(dsn):
    conn = psycopg.connect(dsn, autocommit=True)
    if not conn.execute("SELECT current_database()").fetchone()[0].startswith("dh_waypoint_bench_"):
        conn.close()
        raise RuntimeError("refusing database without dh_waypoint_bench_ prefix")
    conn.execute("SET search_path=catalogue_bench")
    conn.execute("SET statement_timeout='5s'")
    return conn


def timed(conn, sql, params=None):
    start = time.perf_counter()
    result = conn.execute(sql, params).fetchall() if params is not None else conn.execute(sql).fetchall()
    return (time.perf_counter() - start) * 1000, result


def bound(conn, sql, params):
    # psycopg's parameter format reserves %, including SQL's modulo operator.
    return conn.execute(sql.replace("%", "%%").replace("%%s", "%s"), params)


def preflight(conn, args):
    # This path must be on the database volume's filesystem, not a cloud mount.
    disk = shutil.disk_usage(args.disk_path)
    info = {"kind": "preflight", "free_bytes": disk.free,
            "source_sha256":hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            "required_free_bytes": args.min_free_gib * 2**30,
            "cpu_count": os.cpu_count(), "postgres": conn.execute("SELECT version()").fetchone()[0],
            "settings": dict(conn.execute("SELECT name,setting FROM pg_settings WHERE name = ANY(%s)",
                          (["shared_buffers", "work_mem", "maintenance_work_mem", "max_parallel_workers_per_gather",
                            "max_connections", "fsync", "synchronous_commit", "full_page_writes"],)).fetchall())}
    emit(args.out, info)
    if disk.free < args.min_free_gib * 2**30:
        raise RuntimeError("insufficient disk reserve")


def seed(conn, args):
    if not args.disposable:
        raise RuntimeError("seed requires --disposable")
    preflight(conn, args)
    conn.execute("SET statement_timeout=0")
    exists = conn.execute("SELECT to_regclass('catalogue_bench.sentinel')").fetchone()[0]
    if not exists:
        conn.execute(DDL)
    if conn.execute("SELECT format FROM sentinel").fetchall() != [("synthetic-catalogue-v1",)]:
        raise RuntimeError("unrecognized fixture")
    # Permit an earlier fixture to acquire the same raw FTS column as core.
    conn.execute("ALTER TABLE torrents ADD COLUMN IF NOT EXISTS tsv tsvector GENERATED ALWAYS AS (to_tsvector('simple',name)) STORED")
    # Universe independent of requested scale; identities remain stable as it grows.
    conn.execute("""INSERT INTO content SELECT CASE WHEN n%3=0 THEN 'movie' ELSE 'tv_show' END,
      'synthetic',n::text,'Synthetic title'||n,(n%101=0)
      FROM generate_series(1,1001003) n ON CONFLICT DO NOTHING""")
    conn.execute("""INSERT INTO content_attributes SELECT type,source,id,'imdb','id','tt'||id
      FROM content ON CONFLICT DO NOTHING""")
    first = conn.execute("SELECT coalesce(max(n),0)+1 FROM torrents").fetchone()[0]
    count=conn.execute("SELECT count(*) FROM torrents").fetchone()[0]
    if count != first-1:
        raise RuntimeError("incomplete previous parallel seed: recreate disposable fixture")
    bulk=args.rows-count>=1000000 and not args.keep_seed_indexes
    emit(args.out,{"kind":"seed_policy","rows_before":count,"rows_target":args.rows,
                   "workers":args.workers,"bulk_secondary_rebuild":bulk})
    if bulk:
        # Population construction is separate from steady ingestion. Keep all
        # PK/unique/FK constraints, and rebuild only our query indexes afterward.
        for sql in BASE_INDEXES:
            name=sql.split()[2]
            conn.execute('DROP INDEX IF EXISTS '+name)
            emit(args.out,{"kind":"bulk_index_drop","index":name})
    ranges=[(start,min(start+args.batch-1,args.rows)) for start in range(first,args.rows+1,args.batch)]
    def worker(bounds):
        with connect(args.dsn) as batch_conn:
            batch_conn.execute("SET statement_timeout=0")
            seed_range(batch_conn,args,*bounds)
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as executor:
        list(executor.map(worker,ranges))
    for sql in BASE_INDEXES:
        sql = sql.replace("CREATE INDEX ","CREATE INDEX IF NOT EXISTS ")
        start = time.perf_counter()
        conn.execute(sql)
        emit(args.out,{"kind":"index_build","variant":"baseline","sql":sql,"seconds":time.perf_counter()-start})
    conn.execute("VACUUM (ANALYZE)")
    storage(conn,args)


def seed_range(conn,args,start,end):
    if shutil.disk_usage(args.disk_path).free < args.min_free_gib * 2**30:
        raise RuntimeError("disk reserve reached during seed")
    t0 = time.perf_counter()
    with conn.transaction():
        bound(conn, f"""INSERT INTO torrents(n,info_hash,name,size,private,files_count,created_at)
          SELECT n,decode(md5('synthetic-'||n)||substr(md5('tail-'||n),1,8),'hex'),
            'Synthetic title'||family||' release'||n%4096||' season'||n%10||
            ' episode'||n%24||' resolution'||CASE WHEN n%8=0 THEN 2160 ELSE 1080 END||' group'||n%127,
            (1+n%64)*1073741824::bigint,n%199=0,
            CASE WHEN n%100=0 THEN 100 WHEN n%20=0 THEN 10 ELSE 1 END,
            '2025-01-01'::timestamptz+(n%100000)*interval '1 second'
          FROM (SELECT n,{IDENTITY} family FROM generate_series(%s::bigint,%s::bigint) n) x""", (start,end))
        bound(conn, f"""INSERT INTO torrent_contents
          (info_hash,content_type,content_source,content_id,search_string,episodes,video_resolution,
           seeders,published_at,updated_at)
          SELECT info_hash,CASE WHEN {TRUSTED} THEN {TYPE} END,
            CASE WHEN {TRUSTED} THEN 'synthetic' END,
            CASE WHEN {TRUSTED} THEN family::text END,name,
            jsonb_build_object((n%10)::text,CASE WHEN n%11=0 THEN '{{}}'::jsonb
              ELSE jsonb_build_object((n%24)::text,'{{}}'::jsonb) END),
            CASE WHEN n%8=0 THEN '2160p' ELSE '1080p' END,
            CASE WHEN n%7=0 THEN NULL ELSE n%500 END,created_at,created_at
          FROM (SELECT *,{IDENTITY} family FROM torrents WHERE n BETWEEN %s AND %s) x""",(start,end))
        bound(conn, f"""INSERT INTO annotations SELECT info_hash,'synthetic/classifier',
          CASE WHEN (n/20)%10=7 THEN 1 ELSE 2 END,2,
          CASE WHEN (n/20)%10=8 THEN 0.4 ELSE 0.99 END,
          CASE WHEN {TRUSTED} THEN 'accepted' ELSE 'proposed' END,{TYPE},family::text
          FROM (SELECT *,{IDENTITY} family FROM torrents WHERE n BETWEEN %s AND %s) x
          WHERE (n/20)%10<>9""",(start,end))
        conn.execute("""INSERT INTO torrent_files SELECT info_hash,i,
          'synthetic/'||n||'/file-'||i||'.mkv',size/files_count
          FROM torrents CROSS JOIN LATERAL generate_series(1,files_count) i WHERE n BETWEEN %s AND %s""",(start,end))
        bound(conn,"INSERT INTO quarantined SELECT info_hash FROM torrents WHERE n BETWEEN %s AND %s AND n%211=0",(start,end))
    emit(args.out,{"kind":"seed_batch","start":start,"end":end,"seconds":time.perf_counter()-t0})


def storage(conn,args):
    conn.execute("SET statement_timeout='180s'")
    emit(args.out,{"kind":"storage","rows":conn.execute("SELECT count(*) FROM torrents").fetchone()[0],
                  "files":conn.execute("SELECT count(*) FROM torrent_files").fetchone()[0],
                  "relations":conn.execute("""SELECT relname,pg_relation_size(oid),pg_indexes_size(oid),
                    pg_total_relation_size(oid) FROM pg_class WHERE relnamespace='catalogue_bench'::regnamespace
                    AND relkind='r' ORDER BY relname""").fetchall(),
                  "indexes":conn.execute("""SELECT indexrelname,pg_relation_size(indexrelid)
                    FROM pg_stat_user_indexes WHERE schemaname='catalogue_bench' ORDER BY indexrelname""").fetchall()})
    conn.execute("SET statement_timeout='5s'")


def cases(rows):
    fields = "tc.id,tc.info_hash"
    def page(predicate, suffix="LIMIT 100"):
        return f"SELECT {fields} FROM torrent_contents tc WHERE ({predicate}) AND {ELIGIBLE} ORDER BY {ORDER} {suffix}"
    identity = "tc.content_type='tv_show' AND tc.content_source='synthetic' AND tc.content_id='1'"
    text = "tc.tsv @@ 'title1'::tsquery"
    queries = {
      "hash":page("tc.info_hash=decode(md5('synthetic-1234')||substr(md5('tail-1234'),1,8),'hex')"),
      "identity_hot":page(identity),
      "identity_rare":page("tc.content_type='movie' AND tc.content_source='synthetic' AND tc.content_id='123456'"),
      "identity_join":f"SELECT {fields} FROM torrent_contents tc JOIN content c ON (c.type,c.source,c.id)="
        f"(tc.content_type,tc.content_source,tc.content_id) WHERE c.type='tv_show' AND c.source='synthetic' AND c.id='1' AND {ELIGIBLE} ORDER BY {ORDER} LIMIT 100",
      "external_correlated":page("EXISTS (SELECT 1 FROM content_attributes ca WHERE "
        "(ca.content_type,ca.content_source,ca.content_id)=(tc.content_type,tc.content_source,tc.content_id) "
        "AND ca.source='imdb' AND ca.value='tt123456')"),
      "external_identifier":page("EXISTS (SELECT 1 FROM content_attributes ca WHERE "
        "(ca.content_type,ca.content_source,ca.content_id)=(tc.content_type,tc.content_source,tc.content_id) "
        "AND ca.source='imdb' AND ca.key='id' AND ca.value='tt123456')"),
      "external_keyset":page("(tc.content_type,tc.content_source,tc.content_id) IN ("
        "VALUES ('movie','imdb','tt123456') UNION ALL SELECT content_type,content_source,content_id "
        "FROM content_attributes WHERE content_type='movie' AND source='imdb' AND key='id' AND value='tt123456')"),
      "text_hot":page(text),
      "text_rare":page("tc.tsv @@ 'title123456'::tsquery"),
      "text_miss":page("tc.tsv @@ 'unfindabletoken'::tsquery"),
      "type_browse":page("tc.content_type='movie'"),
      "quality":page("tc.content_type='tv_show' AND tc.video_resolution='2160p'"),
      "season_current":page("jsonb_exists(tc.episodes,'3')"),
      "season_indexable":page("tc.episodes @> '{\"3\":{}}'::jsonb"),
      "episode_current":page("(tc.episodes #> '{3}'='{}'::jsonb OR tc.episodes #> '{3}' @> '{\"3\":{}}'::jsonb)"),
      "episode_indexable":page("tc.episodes @> '{\"3\":{\"3\":{}}}'::jsonb OR "
        "(tc.episodes @> '{\"3\":{}}'::jsonb AND tc.episodes #> '{3}'='{}'::jsonb)"),
      "offset":page("true",f"OFFSET {min(rows//2,1000000)} LIMIT 100"),
      # Cursor anchor is an actual deterministic tuple, not a timestamp forecast.
      "keyset":page("(tc.published_at,tc.info_hash,tc.id)<CURSOR_ANCHOR"),
      "grouped":f"SELECT DISTINCT ON (tc.content_type,tc.content_source,tc.content_id) {fields} "
        f"FROM torrent_contents tc WHERE tc.content_id IS NOT NULL AND {ELIGIBLE} "
        "ORDER BY tc.content_type,tc.content_source,tc.content_id,coalesce(tc.seeders,-1) DESC,tc.info_hash LIMIT 100",
      # Same eligible textual set as raw text; never certify fallback as a known identity.
      "enriched_with_fallback":f"SELECT id,info_hash FROM ("
        f"SELECT tc.id,tc.info_hash,tc.published_at FROM torrent_contents tc WHERE ({identity}) AND {ELIGIBLE} "
        "UNION ALL SELECT tc.id,tc.info_hash,tc.published_at FROM torrent_contents tc "
        f"WHERE tc.content_id IS NULL AND ({text}) AND {ELIGIBLE}) all_candidates ORDER BY {ORDER} LIMIT 100",
    }
    queries['enriched_bounded_fallback']=f"SELECT id,info_hash FROM (("
    queries['enriched_bounded_fallback']+=f"SELECT tc.id,tc.info_hash,tc.published_at FROM torrent_contents tc WHERE ({identity}) AND {ELIGIBLE} ORDER BY {ORDER} LIMIT 100) "
    queries['enriched_bounded_fallback']+="UNION ALL (SELECT tc.id,tc.info_hash,tc.published_at FROM torrent_contents tc "
    queries['enriched_bounded_fallback']+=f"WHERE tc.content_id IS NULL AND ({text}) AND {ELIGIBLE} ORDER BY {ORDER} LIMIT 100)) all_candidates ORDER BY {ORDER} LIMIT 100"
    raw_policy=ELIGIBLE.replace('=tc.info_hash','=raw.info_hash')
    def raw_page(predicate):
        return f"SELECT encode(raw.info_hash,'hex') AS id,raw.info_hash FROM torrents raw WHERE ({predicate}) AND {raw_policy} ORDER BY raw.info_hash LIMIT 100"
    queries['raw_text_hot']=raw_page("raw.tsv @@ 'title1'::tsquery")
    queries['raw_text_rare']=raw_page("raw.tsv @@ 'title123456'::tsquery")
    queries['raw_text_miss']=raw_page("raw.tsv @@ 'unfindabletoken'::tsquery")
    queries['raw_keyset']=raw_page("raw.info_hash>decode('8000000000000000000000000000000000000000','hex')")
    # REST v2 uses escaped literal ILIKE, not TSV syntax. Keep these separate
    # from the raw full-text alternative and fetch limit+1 as the API does.
    for name,term in {'hot':'title1','rare':'title123456 ',
                      'miss':'unfindabletoken','wildcard':'title_%'}.items():
        escaped=term.replace('\\','\\\\').replace('%','\\%').replace('_','\\_')
        literal=psycopg.sql.Literal('%'+escaped+'%').as_string()
        queries['raw_substring_'+name]=raw_page('raw.name ILIKE '+literal).replace('LIMIT 100','LIMIT 101')
    return queries


def digest(result):
    return hashlib.sha256(repr([(r[0],bytes(r[1]).hex()) for r in result]).encode()).hexdigest()


def verify_page(conn,args,name,result):
    """An independent mathematical oracle; does not reuse ELIGIBLE SQL."""
    hashes=[bytes(r[1]) for r in result]
    if len(set(r[0] for r in result))!=len(result):
        raise RuntimeError(f"duplicate row in {name}")
    observations=conn.execute("SELECT info_hash,n FROM torrents WHERE info_hash=ANY(%s)",(hashes,)).fetchall()
    by_hash={bytes(h):n for h,n in observations}
    for identifier,hash_value in result:
        n=by_hash[bytes(hash_value)]
        family=1 if n%20==0 else 1000+(n*48271)%1000003
        trusted=(n//20)%10<7
        if n%199==0 or n%211==0 or (trusted and family%101==0):
            raise RuntimeError(f"ineligible result from {name}")
        if name in ['identity_hot','identity_join','identity_episode_quality'] and not (family==1 and trusted):
            raise RuntimeError(f"wrong canonical identity from {name}")
        if name=='identity_episode_quality' and not (n%8==0 and n%10==0 and (n%11==0 or n%24==4)):
            raise RuntimeError('wrong canonical episode/quality candidate')
        if name in ['identity_rare','external_correlated','external_identifier','external_keyset'] and not (family==123456 and trusted):
            raise RuntimeError(f"wrong rare or external identity from {name}")
        if name in ['text_hot','raw_text_hot','enriched_with_fallback','enriched_bounded_fallback'] and family!=1:
            raise RuntimeError(f"wrong text candidate from {name}")
        if name in ['text_rare','raw_text_rare'] and family!=123456:
            raise RuntimeError("wrong rare text candidate")
        if name=='type_browse' and not (trusted and family%3==0):
            raise RuntimeError("wrong movie type")
        if name=='quality' and not (trusted and family%3!=0 and n%8==0):
            raise RuntimeError("wrong TV quality")
        if name.startswith('season_') and n%10!=3:
            raise RuntimeError("wrong season")
        if name.startswith('episode_') and not (n%10==3 and (n%11==0 or n%24==3)):
            raise RuntimeError("wrong episode or pack")
    if name in ['text_miss','raw_text_miss'] and result:
        raise RuntimeError("miss returned results")
    if name.startswith('raw_substring_'):
        if name in ['raw_substring_miss','raw_substring_wildcard'] and result:
            raise RuntimeError('literal substring miss returned results')
        for _,hash_value in result:
            n=by_hash[bytes(hash_value)]
            family=1 if n%20==0 else 1000+(n*48271)%1000003
            if name=='raw_substring_hot' and 'title1' not in 'title'+str(family):
                raise RuntimeError('wrong literal substring candidate')
            if name=='raw_substring_rare' and family!=123456:
                raise RuntimeError('wrong rare literal substring candidate')
    if name in ['identity_rare','external_correlated','external_identifier','external_keyset',
                'text_rare','raw_text_rare','raw_substring_rare']:
        # Invert the fixture's modular identity assignment. This independently
        # checks completeness and ordering, including an incorrectly empty page.
        rows=conn.execute('SELECT max(n) FROM torrents').fetchone()[0]
        first=((123456-1000)*pow(48271,-1,1000003))%1000003
        expected=[]
        for n in range(first or 1000003,rows+1,1000003):
            trusted=(n//20)%10<7
            if n%20==0 or n%199==0 or n%211==0:
                continue
            if name in ['identity_rare','external_correlated','external_identifier','external_keyset'] and not trusted:
                continue
            h=bytes.fromhex(hashlib.md5(('synthetic-'+str(n)).encode()).hexdigest()+
                            hashlib.md5(('tail-'+str(n)).encode()).hexdigest()[:8])
            expected.append((n%100000,h))
        expected_hashes=([h for _,h in sorted(expected,key=lambda r:r[1])] if name.startswith('raw_')
                         else [h for _,h in sorted(expected,reverse=True)])
        if hashes!=expected_hashes[:101 if name=='raw_substring_rare' else 100]:
            raise RuntimeError('rare identity/text page differs from independent complete oracle: '+name)
    if name=='identity_episode_quality':
        rows=conn.execute('SELECT max(n) FROM torrents').fetchone()[0]
        expected=[]
        for n in range(20,rows+1,20):
            if (n//20)%10>=7 or n%199==0 or n%211==0 or n%8!=0 or not (n%11==0 or n%24==4):
                continue
            h=bytes.fromhex(hashlib.md5(('synthetic-'+str(n)).encode()).hexdigest()+
                            hashlib.md5(('tail-'+str(n)).encode()).hexdigest()[:8])
            expected.append((n%100000,h))
        if hashes!=[h for _,h in sorted(expected,reverse=True)[:100]]:
            raise RuntimeError('combined identity/episode/quality page differs from complete oracle')
    emit(args.out,{"kind":"page_correctness","case":name,"checked":len(result),"failures":0})


def correctness(conn,args,queries):
    pairs=[("identity_hot","identity_join"),("external_correlated","external_identifier"),
           ("external_identifier","external_keyset"),("season_current","season_indexable"),
           ("episode_current","episode_indexable"),("text_hot","enriched_with_fallback")]
    # Both directions and complete sets, independent of LIMIT/page representative.
    for left,right in pairs:
        def full(sql):
            return sql.rsplit(" ORDER BY ",1)[0]
        query=f"SELECT count(*) FROM (({full(queries[left])} EXCEPT {full(queries[right])}) UNION ALL "
        query+=f"({full(queries[right])} EXCEPT {full(queries[left])})) differences"
        start=time.perf_counter()
        conn.execute("SET statement_timeout='180s'")
        delta=conn.execute(query).fetchone()[0]
        emit(args.out,{"kind":"correctness","left":left,"right":right,"differences":delta,
                       "seconds":time.perf_counter()-start})
        if delta:
            raise RuntimeError(f"result mismatch {left}/{right}: {delta}")
    raw=conn.execute(queries['text_hot']).fetchall()
    bounded=conn.execute(queries['enriched_bounded_fallback']).fetchall()
    if raw!=bounded:
        raise RuntimeError("bounded fallback changed page or ordering")
    emit(args.out,{"kind":"correctness","left":"text_hot","right":"enriched_bounded_fallback",
                   "page_equal":True,"differences":0})
    left=queries['text_hot'].rsplit(' ORDER BY ',1)[0]
    right=queries['raw_text_hot'].rsplit(' ORDER BY ',1)[0]
    delta=conn.execute(f"SELECT count(*) FROM ((SELECT info_hash FROM ({left}) a EXCEPT SELECT info_hash FROM ({right}) b) "
                       f"UNION ALL (SELECT info_hash FROM ({right}) b EXCEPT SELECT info_hash FROM ({left}) a)) difference").fetchone()[0]
    emit(args.out,{"kind":"correctness","left":"content_name_fts","right":"raw_torrent_name_fts","differences":delta})
    if delta:
        raise RuntimeError("raw and content name indexes disagree")
    if args.rows>=198:
        # Temporarily remove one unclassified projection. Raw catalogue search
        # must still work without waiting for any processor to create that row.
        raw_policy=ELIGIBLE.replace('=tc.info_hash','=raw.info_hash')
        with conn.transaction():
            conn.execute("DELETE FROM torrent_contents WHERE info_hash=(SELECT info_hash FROM torrents WHERE n=198)")
            visible=conn.execute(f"SELECT count(*) FROM torrents raw WHERE raw.n=198 AND {raw_policy}").fetchone()[0]
            if visible!=1:
                raise RuntimeError("raw record lost without content projection")
            raise psycopg.Rollback()
        emit(args.out,{"kind":"raw_without_projection","visible":1})
    # Correctness is assessed while ingestion is stopped, on a stable snapshot.
    unsafe=conn.execute("""SELECT count(*) FROM torrent_contents tc JOIN annotations a USING(info_hash)
      WHERE tc.content_id IS NOT NULL AND (a.source_revision<>a.current_revision OR a.confidence<0.9
      OR a.verification<>'accepted')""").fetchone()[0]
    emit(args.out,{"kind":"authority_check","unsafe_promotions":unsafe})
    if unsafe:
        raise RuntimeError("uncertain or stale annotation promoted")
    conn.execute("SET statement_timeout='5s'")


def writer(args,stop,counter):
    with connect(args.dsn) as conn:
        rng=random.Random(9844)
        maximum=conn.execute("SELECT max(n) FROM torrents").fetchone()[0]
        next_new=maximum+1
        batches=0
        while not stop.is_set():
            lower=rng.randrange(1,max(2,maximum-100))
            start=time.perf_counter()
            try:
                if args.ingest and batches%5==0:
                    seed_range(conn,args,next_new,next_new+99)
                    counter.append({"inserted_rows":100,"ms":(time.perf_counter()-start)*1000})
                    next_new+=100
                    start=time.perf_counter()
                # Crawl re-observation plus changed source metadata. Fenced within fixture.
                with conn.transaction():
                    conn.execute("UPDATE torrents SET created_at=created_at WHERE n BETWEEN %s AND %s",(lower,lower+99))
                    result=conn.execute("""UPDATE torrent_contents SET updated_at=clock_timestamp(),
                      seeders=coalesce(seeders,0)+1 WHERE info_hash IN
                      (SELECT info_hash FROM torrents WHERE n BETWEEN %s AND %s)""",(lower,lower+99))
                counter.append({"rows":result.rowcount,"ms":(time.perf_counter()-start)*1000})
                batches+=1
            except psycopg.Error as err:
                counter.append({"error":err.sqlstate})
                stop.set()


def run(conn,args):
    preflight(conn,args)
    conn.execute("SET statement_timeout='180s'")
    rows=(args.rows if args.cache_state=="first_touch" else conn.execute("SELECT count(*) FROM torrents").fetchone()[0])
    queries=cases(rows)
    if args.cases:
        queries={k:v for k,v in queries.items() if k in args.cases.split(',')}
    else:
        # Preserve the original 25-case mix across all three main scales.
        # The subsequently merged v2 API gets an explicit supplemental profile.
        queries={k:v for k,v in queries.items() if not k.startswith('raw_substring_')}
    # Resolve cursor outside measurement; fetching an anchor is not part of the keyset request.
    if "keyset" in queries:
        anchor_start=time.perf_counter()
        anchor=conn.execute(f"SELECT published_at,info_hash,id FROM torrent_contents ORDER BY {ORDER} "
                            f"OFFSET {min(rows//2,1000000)} LIMIT 1").fetchone()
        literal=psycopg.sql.SQL("({},{},{})").format(*(psycopg.sql.Literal(x) for x in anchor)).as_string(conn)
        queries['keyset']=queries['keyset'].replace('CURSOR_ANCHOR',literal)
        emit(args.out,{"kind":"anchor_setup","rows":rows,"seconds":time.perf_counter()-anchor_start})
    conn.execute("SET statement_timeout='5s'")
    if not args.cases and args.cache_state=="warm":
        correctness(conn,args,queries)
    emit(args.out,{"kind":"run_start","rows":rows,"variant":args.variant,"cache_state":args.cache_state,
                   "samples_per_case":args.samples,"workers":args.workers,"concurrent_updates":args.updates,
                   "queries":queries})
    stop=threading.Event()
    counter=[]
    initial_max=conn.execute("SELECT max(n) FROM torrents").fetchone()[0]
    updater=threading.Thread(target=writer,args=(args,stop,counter)) if args.updates else None
    load_start=time.perf_counter()
    if updater:
        updater.start()
    jobs=[(name,sql,i) for i in range(args.samples) for name,sql in queries.items()]
    timed_out=set()
    if args.cache_state=="warm":
        for name,sql in queries.items():
            try:
                result=conn.execute(sql).fetchall()
                verify_page(conn,args,name,result)
            except psycopg.Error as err:
                emit(args.out,{"kind":"warmup_error","case":name,"sqlstate":err.sqlstate})
                if err.sqlstate=='57014':
                    timed_out.add(name)
    random.Random(4501).shuffle(jobs)
    start=time.perf_counter()
    def worker(part):
        with connect(args.dsn) as worker_conn:
            for name,sql,i in part:
                if name in timed_out:
                    emit(args.out,{"kind":"sample_skipped","case":name,"sample":i,"reason":"prior timeout"})
                    continue
                try:
                    ms,result=timed(worker_conn,sql)
                    emit(args.out,{"kind":"sample","rows":rows,"variant":args.variant,
                      "cache_state":args.cache_state,"updates":args.updates,"case":name,"sample":i,
                      "ms":ms,"count":len(result),"digest":digest(result)})
                except psycopg.Error as err:
                    emit(args.out,{"kind":"sample_error","rows":rows,"variant":args.variant,
                      "cache_state":args.cache_state,"updates":args.updates,"case":name,"sample":i,
                      "sqlstate":err.sqlstate})
                    if err.sqlstate=='57014':
                        timed_out.add(name)
    try:
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as executor:
            futures=[executor.submit(worker,jobs[i::args.workers]) for i in range(args.workers)]
            for future in futures:
                future.result()
    finally:
        stop.set()
        if updater:
            updater.join(timeout=35)
    seconds=time.perf_counter()-start
    emit(args.out,{"kind":"update_summary","variant":args.variant,"rows":rows,
                   "seconds":time.perf_counter()-load_start,"query_seconds":seconds,
                   "updated_rows":sum(x.get('rows',0) for x in counter),
                   "inserted_rows":sum(x.get('inserted_rows',0) for x in counter),"batches":counter})
    if args.ingest:
        # Restore the exact initial corpus before comparing another variant.
        # Remove only this run's synthetic tail, in FK order, within one transaction.
        conn.execute("SET statement_timeout='180s'")
        with conn.transaction():
            for table in ['quarantined','torrent_files','annotations','torrent_contents']:
                conn.execute(f"DELETE FROM {table} WHERE info_hash IN (SELECT info_hash FROM torrents WHERE n>%s)",(initial_max,))
            deleted=conn.execute("DELETE FROM torrents WHERE n>%s",(initial_max,)).rowcount
        emit(args.out,{"kind":"synthetic_tail_cleanup","deleted_torrents":deleted,"initial_max":initial_max})
        conn.execute("SET statement_timeout='5s'")
    if any('error' in batch for batch in counter):
        raise RuntimeError('concurrent writer failed; inspect retained batch errors')
    for name,sql in queries.items():
        if args.cache_state=="first_touch":
            continue
        try:
            plan=conn.execute("EXPLAIN (ANALYZE,BUFFERS,WAL,SETTINGS,FORMAT JSON) "+sql).fetchone()[0]
            emit(args.out,{"kind":"plan","rows":rows,"variant":args.variant,"case":name,"plan":plan})
        except psycopg.Error as err:
            emit(args.out,{"kind":"plan_error","case":name,"sqlstate":err.sqlstate})
    if args.cache_state!='first_touch':
        storage(conn,args)


def indexes(conn,args):
    conn.execute("SET statement_timeout=0")
    if args.variant=='selected':
        # Qualify the compact serving set separately: the raw B-tree cannot
        # implement @@, and current episode predicates do not use whole-JSON GIN.
        conn.execute('CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public')
        statements=[sql for sql in CANDIDATE_INDEXES if not any(x in sql for x in ['tc_episodes_gin','raw_tsv_gin'])]
        statements.append(RAW_SUBSTRING_INDEX)
        statements.append(IDENTITY_PAGE_INDEX)
        for sql in statements:
            start=time.perf_counter()
            statement=sql.replace('CREATE INDEX ','CREATE INDEX CONCURRENTLY IF NOT EXISTS ')
            conn.execute(statement)
            emit(args.out,{"kind":"index_change","variant":args.variant,"sql":statement,"seconds":time.perf_counter()-start})
        for name in ['raw_tsv_btree','raw_tsv_gin','tc_episodes_gin']:
            start=time.perf_counter()
            statement='DROP INDEX CONCURRENTLY IF EXISTS '+name
            conn.execute(statement)
            emit(args.out,{"kind":"index_change","variant":args.variant,"sql":statement,"seconds":time.perf_counter()-start})
        conn.execute('ANALYZE')
        storage(conn,args)
        return
    conn.execute('DROP INDEX IF EXISTS tc_identity_page')
    conn.execute('DROP INDEX IF EXISTS raw_name_trgm')
    conn.execute('CREATE INDEX IF NOT EXISTS raw_tsv_btree ON torrents(tsv)')
    for sql in CANDIDATE_INDEXES:
        name=sql.split()[2]
        statement=(sql.replace("CREATE INDEX ","CREATE INDEX IF NOT EXISTS ") if args.variant=="candidate"
                   else "DROP INDEX IF EXISTS "+name)
        start=time.perf_counter()
        conn.execute(statement)
        emit(args.out,{"kind":"index_change","variant":args.variant,"sql":statement,"seconds":time.perf_counter()-start})
    conn.execute("ANALYZE")
    storage(conn,args)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("command",choices=["seed","run","indexes","storage","reindex"])
    p.add_argument("--dsn",default=os.environ.get("CATALOGUE_BENCH_DSN"))
    p.add_argument("--out",required=True)
    p.add_argument("--rows",type=int,default=2400000)
    p.add_argument("--batch",type=int,default=100000)
    p.add_argument("--keep-seed-indexes",action="store_true",help="retain query indexes during large fixture construction")
    p.add_argument("--samples",type=int,default=30)
    p.add_argument("--workers",type=int,default=1)
    p.add_argument("--updates",action="store_true")
    p.add_argument("--ingest",action="store_true",help="append real synthetic torrent/file transactions during updates, then remove that tail")
    p.add_argument("--disposable",action="store_true")
    p.add_argument("--variant",choices=["baseline","candidate","selected"],default="baseline")
    p.add_argument("--cache-state",choices=["warm","first_touch"],default="warm")
    p.add_argument("--cases")
    p.add_argument("--disk-path",default=".")
    p.add_argument("--min-free-gib",type=int,default=100)
    args=p.parse_args()
    if not args.dsn:
        p.error("set CATALOGUE_BENCH_DSN to the disposable database")
    if not 1 <= args.rows <= 32000000 or not 1 <= args.workers <= 16 or args.samples<1:
        p.error("invalid workload bounds")
    if args.ingest and not args.updates:
        p.error("--ingest requires --updates")
    Path(args.out).parent.mkdir(parents=True,exist_ok=True)
    with connect(args.dsn) as conn:
        if args.command=="seed": seed(conn,args)
        elif args.command=="run": run(conn,args)
        elif args.command=="indexes": indexes(conn,args)
        elif args.command=="storage": storage(conn,args)
        else:
            start=time.perf_counter()
            conn.execute("SET statement_timeout=0")
            conn.execute("REINDEX SCHEMA catalogue_bench")
            emit(args.out,{"kind":"reindex","variant":args.variant,"seconds":time.perf_counter()-start})


if __name__=="__main__":
    main()
