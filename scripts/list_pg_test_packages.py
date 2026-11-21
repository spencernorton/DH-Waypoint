#!/usr/bin/env python3
"""Auto-discover Go packages containing PostgreSQL integration tests and split into shards."""
import argparse
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]

WEIGHT_HINTS = {
    "./internal/processors": 5,
    "./internal/database/search": 4,
    "./internal/serving": 3,
    "./internal/processor": 3,
    "./internal/leanmode": 3,
    "./internal/seeds": 3,
    "./internal/torznab/adapter": 3,
    "./migrations": 2,
}

PG_MARKERS = (
    "DH_WAYPOINT_TEST_POSTGRES_DSN",
    "gorm.io/driver/postgres",
    "github.com/jackc/pgx/v5",
)


def discover_packages():
    pkgs = set()
    for path in ROOT.rglob("*_test.go"):
        rel = path.parent.relative_to(ROOT)
        if any(part.startswith(".") or part.startswith("_") for part in rel.parts):
            continue
        if rel.parts and rel.parts[0] in {"tools", "adapters"}:
            continue
        pkg = "./" + rel.as_posix()
        if pkg in pkgs:
            continue
        if path.name.endswith("_postgres_test.go"):
            pkgs.add(pkg)
            continue
        text = path.read_text(errors="ignore")
        if any(m in text for m in PG_MARKERS):
            pkgs.add(pkg)
    return sorted(pkgs)


def split_shards(pkgs, total_shards):
    ordered = sorted(pkgs, key=lambda p: (-WEIGHT_HINTS.get(p, 1), p))
    buckets = [[] for _ in range(total_shards)]
    loads = [0] * total_shards
    for pkg in ordered:
        idx = min(range(total_shards), key=lambda i: (loads[i], len(buckets[i])))
        buckets[idx].append(pkg)
        loads[idx] += WEIGHT_HINTS.get(pkg, 1)
    return [sorted(b) for b in buckets]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--shards", type=int, default=1)
    parser.add_argument("--index", type=int, default=0, help="0-based shard index")
    args = parser.parse_args()
    if args.shards < 1 or not (0 <= args.index < args.shards):
        parser.error("invalid --shards/--index")
    pkgs = discover_packages()
    if not pkgs:
        sys.exit("no PostgreSQL test packages discovered")
    selected = split_shards(pkgs, args.shards)[args.index]
    print(" ".join(selected))


if __name__ == "__main__":
    main()
