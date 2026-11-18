#!/usr/bin/env python3
import hashlib
import http.server
import socketserver
import threading
import unittest

from scripts.fetch_imported_metadata import (
    extract_bencode_info_slice,
    fetch_from_templates,
    post_verified_torrent,
    verify_torrent_bytes,
)


def make_synthetic_torrent(name: bytes = b"Synthetic.Release.mkv") -> tuple[str, bytes]:
    piece = b"\x00" * 16384
    piece_sha1 = hashlib.sha1(piece).digest()
    info = (
        b"d6:lengthi16384e4:name"
        + str(len(name)).encode("ascii")
        + b":"
        + name
        + b"12:piece lengthi16384e6:pieces20:"
        + piece_sha1
        + b"e"
    )
    info_hash = hashlib.sha1(info).hexdigest()
    outer = b"d8:announce25:http://tracker.invalid:804:info" + info + b"e"
    return info_hash, outer


class FetchImportedMetadataTest(unittest.TestCase):
    def test_extract_and_verify_info_slice(self) -> None:
        info_hash, raw = make_synthetic_torrent()
        sliced = extract_bencode_info_slice(raw)
        self.assertEqual(hashlib.sha1(sliced).hexdigest(), info_hash)
        self.assertTrue(verify_torrent_bytes(raw, info_hash))
        self.assertFalse(verify_torrent_bytes(raw, "0" * 40))

    def test_fetch_and_post_roundtrip(self) -> None:
        info_hash, raw = make_synthetic_torrent()
        received: list[tuple[str, bytes]] = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, fmt: str, *args: object) -> None:
                return

            def do_GET(self) -> None:
                if self.path == f"/cache/{info_hash.upper()}.torrent":
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(raw)
                else:
                    self.send_response(404)
                    self.end_headers()

            def do_POST(self) -> None:
                ln = int(self.headers.get("Content-Length", "0"))
                received.append((self.path, self.rfile.read(ln)))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"ok":true}')

        with socketserver.TCPServer(("127.0.0.1", 0), Handler) as httpd:
            port = httpd.server_address[1]
            t = threading.Thread(target=httpd.serve_forever, daemon=True)
            t.start()
            try:
                fetched = fetch_from_templates(
                    info_hash, [f"http://127.0.0.1:{port}/cache/{{HASH}}.torrent"], 2.0
                )
                self.assertEqual(fetched, raw)
                ok, status = post_verified_torrent(
                    f"http://127.0.0.1:{port}", info_hash, fetched, "test-key", "backfill-1", 2.0
                )
                self.assertTrue(ok)
                self.assertEqual(status, 200)
                self.assertEqual(received[0][0], f"/import/torrent?hash={info_hash}")
                self.assertEqual(received[0][1], raw)
            finally:
                httpd.shutdown()


if __name__ == "__main__":
    unittest.main()
