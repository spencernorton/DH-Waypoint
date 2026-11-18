#!/usr/bin/env python3
"""Fetch verified .torrent metadata for names-only imported infohashes.

Reads hex infohashes (from a newline-delimited file or stdin) corresponding to
catalogue rows in files_status='no_info', retrieves candidate .torrent payloads
from configured HTTP cache templates, verifies that the canonical bencoded info
dictionary hashes to the expected SHA-1 infohash before upload, and submits
verified payloads to DH-Waypoint's /import/torrent endpoint.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Iterable


MAX_TORRENT_BYTES = 20 * 1024 * 1024


def extract_bencode_info_slice(data: bytes) -> bytes:
    """Return the exact byte slice of top-level dictionary key b'4:info'."""
    if not data or data[0:1] != b"d":
        raise ValueError("torrent payload must start with bencoded dictionary")

    def skip_value(pos: int) -> int:
        if pos >= len(data):
            raise ValueError("unexpected EOF in bencode")
        tag = data[pos : pos + 1]
        if tag == b"i":
            end = data.find(b"e", pos + 1)
            if end == -1:
                raise ValueError("unterminated integer")
            return end + 1
        if tag == b"l" or tag == b"d":
            cur = pos + 1
            while cur < len(data) and data[cur : cur + 1] != b"e":
                if tag == b"d":
                    cur = skip_string(cur)
                cur = skip_value(cur)
            if cur >= len(data):
                raise ValueError("unterminated container")
            return cur + 1
        return skip_string(pos)

    def skip_string(pos: int) -> int:
        colon = data.find(b":", pos)
        if colon == -1:
            raise ValueError("invalid byte string length")
        length = int(data[pos:colon].decode("ascii"))
        if length < 0 or colon + 1 + length > len(data):
            raise ValueError("byte string exceeds buffer")
        return colon + 1 + length

    cur = 1
    while cur < len(data) and data[cur : cur + 1] != b"e":
        key_end = skip_string(cur)
        colon = data.find(b":", cur)
        key = data[colon + 1 : key_end]
        val_start = key_end
        val_end = skip_value(val_start)
        if key == b"info":
            return data[val_start:val_end]
        cur = val_end
    raise ValueError("missing info dictionary")


def verify_torrent_bytes(raw: bytes, expected_hex: str) -> bool:
    if not raw or len(raw) > MAX_TORRENT_BYTES:
        return False
    norm = expected_hex.strip().lower()
    if len(norm) != 40:
        return False
    try:
        info_slice = extract_bencode_info_slice(raw)
    except ValueError:
        return False
    return hashlib.sha1(info_slice).hexdigest().lower() == norm


def fetch_from_templates(
    info_hash: str, templates: list[str], timeout: float
) -> bytes | None:
    upper = info_hash.upper()
    lower = info_hash.lower()
    for tmpl in templates:
        url = tmpl.replace("{HASH}", upper).replace("{hash}", lower)
        parsed = urllib.parse.urlparse(url)
        if parsed.scheme not in ("http", "https"):
            continue
        req = urllib.request.Request(
            url, headers={"User-Agent": "dh-waypoint-metadata-fetcher/1.0"}
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                if resp.status != 200:
                    continue
                body = resp.read(MAX_TORRENT_BYTES + 1)
                if verify_torrent_bytes(body, lower):
                    return body
        except (urllib.error.URLError, TimeoutError, OSError):
            continue
    return None


def post_verified_torrent(
    endpoint: str,
    info_hash: str,
    payload: bytes,
    api_key: str,
    import_id: str,
    timeout: float,
) -> tuple[bool, int]:
    base = endpoint.rstrip("/")
    url = f"{base}/import/torrent?hash={urllib.parse.quote(info_hash.lower())}"
    headers = {
        "Content-Type": "application/x-bittorrent",
        "X-Import-Id": import_id,
    }
    if api_key:
        headers["X-Api-Key"] = api_key
    req = urllib.request.Request(url, data=payload, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return (200 <= resp.status < 300), resp.status
    except urllib.error.HTTPError as exc:
        return False, exc.code


def iter_hashes(source: Iterable[str]) -> Iterable[str]:
    for line in source:
        h = line.strip().lower()
        if len(h) == 40 and all(c in "0123456789abcdef" for c in h):
            yield h


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--hashes-file", help="Path to file of 40-hex infohashes (default: stdin)")
    parser.add_argument(
        "--source-template",
        action="append",
        required=True,
        help="HTTP(S) URL template containing {HASH} or {hash}",
    )
    parser.add_argument("--endpoint", default="http://127.0.0.1:3333", help="DH-Waypoint base URL")
    parser.add_argument("--import-id", default="metadata-backfill", help="X-Import-Id header value")
    parser.add_argument("--api-key-env", default="DH_WAYPOINT_API_KEY", help="Env var holding operator API key")
    parser.add_argument("--checkpoint-file", help="Optional path storing already processed hashes")
    parser.add_argument("--rate-limit-ms", type=int, default=200, help="Delay between external fetches")
    parser.add_argument("--timeout", type=float, default=10.0, help="Per-request HTTP timeout seconds")
    parser.add_argument("--dry-run", action="store_true", help="Verify payloads without POSTing to /import/torrent")
    args = parser.parse_args(argv)

    done: set[str] = set()
    cp_path = Path(args.checkpoint_file) if args.checkpoint_file else None
    if cp_path and cp_path.exists():
        done = {line.strip().lower() for line in cp_path.read_text().splitlines() if line.strip()}

    api_key = os.environ.get(args.api_key_env, "")
    lines = Path(args.hashes_file).read_text().splitlines() if args.hashes_file else sys.stdin
    stats = {"checked": 0, "verified": 0, "imported": 0, "missed": 0}

    for h in iter_hashes(lines):
        if h in done:
            continue
        stats["checked"] += 1
        payload = fetch_from_templates(h, args.source_template, args.timeout)
        if payload is None:
            stats["missed"] += 1
        else:
            stats["verified"] += 1
            if args.dry_run:
                ok = True
            else:
                ok, _ = post_verified_torrent(args.endpoint, h, payload, api_key, args.import_id, args.timeout)
            if ok:
                stats["imported"] += 1
                if cp_path:
                    with cp_path.open("a", encoding="utf-8") as fh:
                        fh.write(h + "\n")
        if args.rate_limit_ms > 0:
            time.sleep(args.rate_limit_ms / 1000.0)

    print(json.dumps(stats))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
