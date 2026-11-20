#!/usr/bin/env bash
# Migrate a DH-Waypoint PostgreSQL 16/17 database to ParadeDB pg_search (PG 18+)
# using logical custom-format pg_dump + parallel pg_restore so all indexes are
# rebuilt cleanly across libc/collation boundaries (e.g. Alpine musl -> Debian
# glibc), verify exact row-count and publication/approval digest parity, and
# optionally activate + verify BM25 Top-K search plans.
#
# Never prints credentials or full DSNs.
set -euo pipefail

SOURCE_DSN="${SOURCE_DSN:-}"
TARGET_DSN="${TARGET_DSN:-}"
BM25_DSN="${BM25_DSN:-}"
WORK_DIR=""
KEEP_DUMP=0
JOBS=4
COMPRESS=1
FORMAT="custom"
PG_BIN=""
PG_CONTAINER=""
CONTAINER_DUMP_PATH="/tmp/dhw-paradedb-migrate.dump"
DH_WAYPOINT_BIN=""
SKIP_BM25=0
FORCE_CLEAN=0
DRY_RUN=0
REPORT_JSON=""

usage() {
  cat <<'EOF'
Usage: scripts/migrate-to-paradedb.sh --source-dsn <dsn> --target-dsn <dsn> [options]

Options:
  --source-dsn <dsn>         Source PostgreSQL 16/17 DSN (or env SOURCE_DSN)
  --target-dsn <dsn>         Target ParadeDB PG 18+ DSN (or env TARGET_DSN)
  --work-dir <dir>           Directory for temporary custom dump (default: mktemp -d)
  --keep-dump                Keep custom dump artifact after completion
  --jobs <N>                 Parallel worker count for pg_restore (and pg_dump when --format directory; default: 4)
  --format <custom|directory> pg_dump format: custom (single file, default) or directory (parallel dump for multi-million-row DBs)
  --compress <0-9>           pg_dump compression level (default: 1)
  --pg-bin <dir>             Directory holding PG 18+ pg_dump/pg_restore/psql
  --pg-container <name>      Run pg_dump/pg_restore/psql inside this PG 18+ container
  --container-dump-path <p>  Dump path inside --pg-container (default: /tmp/dhw-paradedb-migrate.dump)
  --dh-waypoint-bin <path>   Path to dh-waypoint binary for BM25 enable + verify
  --bm25-dsn <dsn>           Optional host-reachable DSN for dh-waypoint (defaults to --target-dsn)
  --skip-bm25                Skip dh-waypoint search bm25 enable and verify steps
  --force-clean              Allow restoring into a target DB that already has tables
  --dry-run                  Validate connectivity, versions, and pg_search without writing
  --report-json <path>       Write machine-readable timing and parity JSON report
  -h, --help                 Show this help message
EOF
}

log() {
  printf '[migrate-to-paradedb] %s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

die() {
  log "ERROR: $*"
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-dsn) SOURCE_DSN="$2"; shift 2 ;;
    --target-dsn) TARGET_DSN="$2"; shift 2 ;;
    --work-dir) WORK_DIR="$2"; shift 2 ;;
    --keep-dump) KEEP_DUMP=1; shift ;;
    --jobs|-j) JOBS="$2"; shift 2 ;;
    --format|-F) FORMAT="$2"; shift 2 ;;
    --compress|-Z) COMPRESS="$2"; shift 2 ;;
    --pg-bin) PG_BIN="$2"; shift 2 ;;
    --pg-container) PG_CONTAINER="$2"; shift 2 ;;
    --container-dump-path) CONTAINER_DUMP_PATH="$2"; shift 2 ;;
    --dh-waypoint-bin) DH_WAYPOINT_BIN="$2"; shift 2 ;;
    --bm25-dsn) BM25_DSN="$2"; shift 2 ;;
    --skip-bm25) SKIP_BM25=1; shift ;;
    --force-clean) FORCE_CLEAN=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --report-json) REPORT_JSON="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[[ -n "$SOURCE_DSN" ]] || die "--source-dsn (or SOURCE_DSN) is required"
[[ -n "$TARGET_DSN" ]] || die "--target-dsn (or TARGET_DSN) is required"
[[ "$JOBS" =~ ^[1-9][0-9]*$ ]] || die "--jobs must be a positive integer"
[[ "$FORMAT" == "custom" || "$FORMAT" == "directory" ]] || die "--format must be custom or directory"
[[ "$COMPRESS" =~ ^[0-9]$ ]] || die "--compress must be 0..9"

CREATED_WORK_DIR=0
DUMP_FILE=""
cleanup() {
  if [[ "$KEEP_DUMP" -eq 0 ]]; then
    if [[ -n "$PG_CONTAINER" ]]; then
      docker exec "$PG_CONTAINER" rm -rf "$CONTAINER_DUMP_PATH" >/dev/null 2>&1 || true
    elif [[ -n "$DUMP_FILE" && -e "$DUMP_FILE" ]]; then
      rm -rf "$DUMP_FILE" >/dev/null 2>&1 || true
    fi
    if [[ "$CREATED_WORK_DIR" -eq 1 && -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
      rmdir "$WORK_DIR" >/dev/null 2>&1 || true
    fi
  fi
}
trap cleanup EXIT

run_pg() {
  local tool="$1"
  shift
  if [[ -n "$PG_CONTAINER" ]]; then
    docker exec -e PGCONNECT_TIMEOUT=10 "$PG_CONTAINER" "$tool" "$@"
  elif [[ -n "$PG_BIN" ]]; then
    PGCONNECT_TIMEOUT=10 "${PG_BIN}/${tool}" "$@"
  else
    PGCONNECT_TIMEOUT=10 "$tool" "$@"
  fi
}

psql_scalar() {
  local dsn="$1"
  local sql="$2"
  run_pg psql "$dsn" -X -v ON_ERROR_STOP=1 -tA -c "$sql"
}

now_ms() {
  python3 -c 'import time; print(int(time.time()*1000))'
}

elapsed_sec() {
  local start_ms="$1"
  local end_ms="$2"
  python3 - "$start_ms" "$end_ms" <<'PY'
import sys
print(f"{(int(sys.argv[2]) - int(sys.argv[1])) / 1000.0:.2f}")
PY
}

# Pre-flight validation
log "Validating source and target PostgreSQL connectivity and versions..."
SRC_VER="$(psql_scalar "$SOURCE_DSN" "SHOW server_version;")"
TGT_VER="$(psql_scalar "$TARGET_DSN" "SHOW server_version;")"
TGT_MAJOR="$(psql_scalar "$TARGET_DSN" "SELECT current_setting('server_version_num')::int / 10000;")"
SRC_SCHEMA="$(psql_scalar "$SOURCE_DSN" "SELECT COALESCE((SELECT max(version_id) FROM goose_db_version WHERE is_applied), 0);")"
SRC_SIZE_BYTES="$(psql_scalar "$SOURCE_DSN" "SELECT pg_database_size(current_database())::bigint;")"
PG_SEARCH_AVAIL="$(psql_scalar "$TARGET_DSN" "SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'pg_search');")"
PRELOAD_LIBS="$(psql_scalar "$TARGET_DSN" "SHOW shared_preload_libraries;")"
TGT_HAS_TORRENTS="$(psql_scalar "$TARGET_DSN" "SELECT to_regclass('public.torrents') IS NOT NULL;")"

log "Source PG=${SRC_VER} schema=${SRC_SCHEMA} db_bytes=${SRC_SIZE_BYTES}"
log "Target PG=${TGT_VER} (major=${TGT_MAJOR}) pg_search_available=${PG_SEARCH_AVAIL} preload='${PRELOAD_LIBS}'"

if [[ "$TGT_MAJOR" -lt 18 ]]; then
  die "target PostgreSQL major version is ${TGT_MAJOR}; ParadeDB pg_search 0.26.6 requires PG 18+"
fi
if [[ "$PG_SEARCH_AVAIL" != "t" ]]; then
  die "pg_search extension is not available on target server; use paradedb/paradedb:0.26.6-pg18"
fi
if [[ ",${PRELOAD_LIBS}," != *",pg_search,"* ]]; then
  die "target shared_preload_libraries ('${PRELOAD_LIBS}') does not include pg_search"
fi
if [[ "$TGT_HAS_TORRENTS" == "t" && "$FORCE_CLEAN" -ne 1 ]]; then
  die "target database already contains public.torrents; point to an empty database or pass --force-clean"
fi
if [[ "$SRC_SCHEMA" -lt 81 ]]; then
  log "WARNING: source schema is ${SRC_SCHEMA} (< 81); migrating to schema 81 and compacting torrent_file_bundles before cutover shrinks dump/restore time significantly."
fi

EXTRA_ROLES="$(psql_scalar "$SOURCE_DSN" "SELECT COALESCE(string_agg(rolname, ',' ORDER BY rolname), '') FROM pg_roles WHERE rolname !~ '^pg_' AND rolname NOT IN ('postgres', current_user);")"
if [[ -n "$EXTRA_ROLES" ]]; then
  log "WARNING: source cluster defines additional PostgreSQL role(s) (${EXTRA_ROLES}). Because pg_dump runs with --no-owner --no-privileges, transfer cluster roles with 'pg_dumpall --roles-only' and replay schema/object GRANT statements after restore if external services connect with those roles."
fi

if [[ "$DRY_RUN" -eq 1 ]]; then
  log "Dry-run pre-flight checks passed. No data written."
  exit 0
fi

# Prepare dump location
if [[ -n "$PG_CONTAINER" ]]; then
  DUMP_FILE="$CONTAINER_DUMP_PATH"
else
  if [[ -z "$WORK_DIR" ]]; then
    WORK_DIR="$(mktemp -d)"
    CREATED_WORK_DIR=1
  else
    mkdir -p "$WORK_DIR"
  fi
  DUMP_FILE="${WORK_DIR}/dhw-paradedb-migrate.dump"
fi

# Step 1: Logical dump (custom single-file or parallel directory format)
DUMP_ARGS=(
  --format="$FORMAT"
  --compress="$COMPRESS"
  --no-owner
  --no-privileges
  --file="$DUMP_FILE"
)
if [[ "$FORMAT" == "directory" ]]; then
  DUMP_ARGS+=(--jobs="$JOBS")
  if [[ -n "$PG_CONTAINER" ]]; then
    docker exec "$PG_CONTAINER" rm -rf "$DUMP_FILE"
  else
    rm -rf "$DUMP_FILE"
  fi
fi
log "Step 1/5: Dumping source database (format=${FORMAT}, jobs=${JOBS}, compression=${COMPRESS})..."
T_DUMP_START="$(now_ms)"
run_pg pg_dump "$SOURCE_DSN" "${DUMP_ARGS[@]}"
T_DUMP_END="$(now_ms)"
DUMP_SEC="$(elapsed_sec "$T_DUMP_START" "$T_DUMP_END")"

if [[ -n "$PG_CONTAINER" ]]; then
  if [[ "$FORMAT" == "directory" ]]; then
    DUMP_BYTES="$(docker exec "$PG_CONTAINER" du -sb "$DUMP_FILE" | awk '{print $1}')"
  else
    DUMP_BYTES="$(docker exec "$PG_CONTAINER" stat -c '%s' "$DUMP_FILE")"
  fi
else
  DUMP_BYTES="$(python3 - "$DUMP_FILE" <<'PY'
import os, sys
p = sys.argv[1]
if os.path.isdir(p):
    total = 0
    for r, _, files in os.walk(p):
        for fn in files:
            total += os.path.getsize(os.path.join(r, fn))
    print(total)
else:
    print(os.path.getsize(p))
PY
)"
fi
log "Dump completed in ${DUMP_SEC}s (${DUMP_BYTES} bytes)."

# Step 2: Parallel restore into ParadeDB PG 18
log "Step 2/5: Restoring into target database with jobs=${JOBS}..."
run_pg psql "$TARGET_DSN" -X -v ON_ERROR_STOP=1 -c "CREATE EXTENSION IF NOT EXISTS pg_trgm; CREATE EXTENSION IF NOT EXISTS btree_gin;" >/dev/null

RESTORE_ARGS=(
  --dbname="$TARGET_DSN"
  --jobs="$JOBS"
  --no-owner
  --no-privileges
  --exit-on-error
)
if [[ "$FORCE_CLEAN" -eq 1 ]]; then
  RESTORE_ARGS+=(--clean --if-exists)
fi

T_RESTORE_START="$(now_ms)"
run_pg pg_restore "${RESTORE_ARGS[@]}" "$DUMP_FILE"
T_RESTORE_END="$(now_ms)"
RESTORE_SEC="$(elapsed_sec "$T_RESTORE_START" "$T_RESTORE_END")"
log "Restore completed in ${RESTORE_SEC}s."

# Step 3: ANALYZE target
log "Step 3/5: Running ANALYZE on target database..."
T_ANALYZE_START="$(now_ms)"
run_pg psql "$TARGET_DSN" -X -v ON_ERROR_STOP=1 -c "ANALYZE;" >/dev/null
T_ANALYZE_END="$(now_ms)"
ANALYZE_SEC="$(elapsed_sec "$T_ANALYZE_START" "$T_ANALYZE_END")"
log "ANALYZE completed in ${ANALYZE_SEC}s."

# Step 4: Automated exact row-count & approval/publication digest parity check
log "Step 4/5: Verifying exact row-count and publication/approval parity..."
T_PARITY_START="$(now_ms)"
CORE_TABLES=(
  torrents
  torrent_contents
  torrent_files
  torrent_file_bundles
  content
  torrents_torrent_sources
  processor_publication_sources
  processor_publication_reviews
  processor_payload_reviews
  processor_exclusions
  processor_publication_holds
  junkpurge_quarantine
  key_values
)

PARITY_FAIL=0
PARITY_LINES=""
for tbl in "${CORE_TABLES[@]}"; do
  src_exists="$(psql_scalar "$SOURCE_DSN" "SELECT to_regclass('public.${tbl}') IS NOT NULL;")"
  if [[ "$src_exists" != "t" ]]; then
    continue
  fi
  src_cnt="$(psql_scalar "$SOURCE_DSN" "SELECT count(*) FROM public.${tbl};")"
  tgt_cnt="$(psql_scalar "$TARGET_DSN" "SELECT count(*) FROM public.${tbl};")"
  PARITY_LINES="${PARITY_LINES}${tbl}:${src_cnt}:${tgt_cnt}"$'\n'
  if [[ "$src_cnt" != "$tgt_cnt" ]]; then
    log "PARITY MISMATCH on ${tbl}: source=${src_cnt} target=${tgt_cnt}"
    PARITY_FAIL=1
  else
    log "  parity OK: ${tbl} = ${tgt_cnt} rows"
  fi
done

# Publication revision sum & approved payload review count parity
if [[ "$(psql_scalar "$SOURCE_DSN" "SELECT to_regclass('public.processor_publication_sources') IS NOT NULL;")" == "t" ]]; then
  src_rev_sum="$(psql_scalar "$SOURCE_DSN" "SELECT COALESCE(sum(revision), 0)::bigint FROM public.processor_publication_sources;")"
  tgt_rev_sum="$(psql_scalar "$TARGET_DSN" "SELECT COALESCE(sum(revision), 0)::bigint FROM public.processor_publication_sources;")"
  if [[ "$src_rev_sum" != "$tgt_rev_sum" ]]; then
    log "PARITY MISMATCH on processor_publication_sources.sum(revision): source=${src_rev_sum} target=${tgt_rev_sum}"
    PARITY_FAIL=1
  else
    log "  parity OK: processor_publication_sources sum(revision) = ${tgt_rev_sum}"
  fi
fi

if [[ "$(psql_scalar "$SOURCE_DSN" "SELECT to_regclass('public.processor_payload_reviews') IS NOT NULL;")" == "t" ]]; then
  src_app="$(psql_scalar "$SOURCE_DSN" "SELECT count(*) FILTER (WHERE approved) FROM public.processor_payload_reviews;")"
  tgt_app="$(psql_scalar "$TARGET_DSN" "SELECT count(*) FILTER (WHERE approved) FROM public.processor_payload_reviews;")"
  if [[ "$src_app" != "$tgt_app" ]]; then
    log "PARITY MISMATCH on processor_payload_reviews approved count: source=${src_app} target=${tgt_app}"
    PARITY_FAIL=1
  else
    log "  parity OK: processor_payload_reviews approved = ${tgt_app}"
  fi
fi

if [[ "$PARITY_FAIL" -ne 0 ]]; then
  die "parity verification failed; aborting before BM25 activation"
fi
T_PARITY_END="$(now_ms)"
PARITY_SEC="$(elapsed_sec "$T_PARITY_START" "$T_PARITY_END")"
log "Parity verification passed in ${PARITY_SEC}s."

# Step 5: Opt-in BM25 enable & TopK plan verify
BM25_ENABLE_SEC="0.00"
BM25_VERIFY_SEC="0.00"
BM25_ENABLED="false"
BM25_INDEX_BYTES="0"

if [[ "$SKIP_BM25" -eq 0 ]]; then
  if [[ -z "$DH_WAYPOINT_BIN" ]]; then
    DH_WAYPOINT_BIN="$(command -v dh-waypoint || true)"
  fi
  if [[ -n "$DH_WAYPOINT_BIN" ]]; then
    log "Step 5/5: Enabling and verifying ParadeDB BM25 via ${DH_WAYPOINT_BIN}..."
    # Parse target DSN into POSTGRES_* env vars without echoing secrets.
    eval "$(
      TARGET_DSN="${BM25_DSN:-$TARGET_DSN}" python3 - <<'PY'
import os, shlex
from urllib.parse import urlparse, parse_qs, unquote
u = urlparse(os.environ["TARGET_DSN"])
qs = parse_qs(u.query)
pairs = {
    "POSTGRES_HOST": u.hostname or "127.0.0.1",
    "POSTGRES_PORT": str(u.port or 5432),
    "POSTGRES_NAME": unquote((u.path or "/bitmagnet").lstrip("/") or "bitmagnet"),
    "POSTGRES_USER": unquote(u.username or "postgres"),
    "POSTGRES_PASSWORD": unquote(u.password or ""),
}
if "sslmode" in qs and qs["sslmode"]:
    pairs["POSTGRES_SSL_MODE"] = qs["sslmode"][0]
for k, v in pairs.items():
    print(f"export {k}={shlex.quote(v)}")
PY
    )"

    T_BM25_START="$(now_ms)"
    "$DH_WAYPOINT_BIN" search bm25 enable
    T_BM25_END="$(now_ms)"
    BM25_ENABLE_SEC="$(elapsed_sec "$T_BM25_START" "$T_BM25_END")"

    T_VER_START="$(now_ms)"
    "$DH_WAYPOINT_BIN" search bm25 verify --verbose
    T_VER_END="$(now_ms)"
    BM25_VERIFY_SEC="$(elapsed_sec "$T_VER_START" "$T_VER_END")"

    BM25_ENABLED="true"
    BM25_INDEX_BYTES="$(psql_scalar "$TARGET_DSN" "SELECT COALESCE(pg_relation_size(to_regclass('torrent_contents_bm25_idx')), 0)::bigint;")"
    log "BM25 enabled in ${BM25_ENABLE_SEC}s (index=${BM25_INDEX_BYTES} bytes) and verified in ${BM25_VERIFY_SEC}s."
  else
    log "Step 5/5: dh-waypoint binary not found (pass --dh-waypoint-bin or run 'dh-waypoint search bm25 enable && dh-waypoint search bm25 verify --verbose' manually)."
  fi
else
  log "Step 5/5: Skipped BM25 activation (--skip-bm25)."
fi

TGT_SIZE_BYTES="$(psql_scalar "$TARGET_DSN" "SELECT pg_database_size(current_database())::bigint;")"
INVALID_INDEXES="$(psql_scalar "$TARGET_DSN" "SELECT count(*) FROM pg_index WHERE NOT indisvalid OR NOT indisready;")"
if [[ "$INVALID_INDEXES" != "0" ]]; then
  die "target database has ${INVALID_INDEXES} invalid or unready index(es)"
fi

if [[ -n "$REPORT_JSON" ]]; then
  PARITY_LINES="$PARITY_LINES" \
  SRC_VER="$SRC_VER" TGT_VER="$TGT_VER" SRC_SCHEMA="$SRC_SCHEMA" \
  SRC_SIZE_BYTES="$SRC_SIZE_BYTES" TGT_SIZE_BYTES="$TGT_SIZE_BYTES" \
  DUMP_BYTES="$DUMP_BYTES" DUMP_SEC="$DUMP_SEC" RESTORE_SEC="$RESTORE_SEC" \
  ANALYZE_SEC="$ANALYZE_SEC" PARITY_SEC="$PARITY_SEC" \
  BM25_ENABLED="$BM25_ENABLED" BM25_ENABLE_SEC="$BM25_ENABLE_SEC" \
  BM25_VERIFY_SEC="$BM25_VERIFY_SEC" BM25_INDEX_BYTES="$BM25_INDEX_BYTES" \
  JOBS="$JOBS" COMPRESS="$COMPRESS" INVALID_INDEXES="$INVALID_INDEXES" \
  python3 - "$REPORT_JSON" <<'PY'
import json, os, sys
tables = {}
for line in os.environ.get("PARITY_LINES", "").splitlines():
    if not line.strip():
        continue
    name, sc, tc = line.split(":")
    tables[name] = {"source_rows": int(sc), "target_rows": int(tc), "match": sc == tc}
report = {
    "source_pg_version": os.environ["SRC_VER"],
    "target_pg_version": os.environ["TGT_VER"],
    "source_schema_version": int(os.environ["SRC_SCHEMA"]),
    "jobs": int(os.environ["JOBS"]),
    "compress_level": int(os.environ["COMPRESS"]),
    "source_db_bytes": int(os.environ["SRC_SIZE_BYTES"]),
    "target_db_bytes": int(os.environ["TGT_SIZE_BYTES"]),
    "dump_bytes": int(os.environ["DUMP_BYTES"]),
    "bm25_index_bytes": int(os.environ["BM25_INDEX_BYTES"]),
    "bm25_enabled": os.environ["BM25_ENABLED"] == "true",
    "invalid_indexes": int(os.environ["INVALID_INDEXES"]),
    "timings_sec": {
        "dump": float(os.environ["DUMP_SEC"]),
        "restore": float(os.environ["RESTORE_SEC"]),
        "analyze": float(os.environ["ANALYZE_SEC"]),
        "parity": float(os.environ["PARITY_SEC"]),
        "bm25_enable": float(os.environ["BM25_ENABLE_SEC"]),
        "bm25_verify": float(os.environ["BM25_VERIFY_SEC"]),
    },
    "parity": tables,
}
with open(sys.argv[1], "w", encoding="utf-8") as fh:
    json.dump(report, fh, indent=2, sort_keys=True)
    fh.write("\n")
PY
  log "Wrote migration report to ${REPORT_JSON}"
fi

log "Migration to ParadeDB PG ${TGT_MAJOR} complete (target_db_bytes=${TGT_SIZE_BYTES}, invalid_indexes=${INVALID_INDEXES})."
