#!/usr/bin/env python3
"""Unit tests for scripts/sync_cnb_release.py against a local stub HTTP server."""
import http.server
import json
from pathlib import Path
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import sync_cnb_release as sync


class FakeCNBServer:
    def __init__(self, existing_release_id=None):
        self.existing_release_id = existing_release_id
        self.created_payloads = []
        self.upload_order = []
        self.uploaded_bytes = {}
        self.confirmed = []
        self.fail_first_upload = False
        self._failed_once = False
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, format, *args):
                pass

            def _send_json(self, code, obj):
                data = json.dumps(obj).encode("utf-8")
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):
                if self.headers.get("Authorization") != "Bearer test-token":
                    self._send_json(401, {"error": "unauthorized"})
                    return
                if self.path == "/owner/repo/-/releases/tags/v1.1.4":
                    if outer.existing_release_id:
                        self._send_json(200, {"id": outer.existing_release_id})
                    else:
                        self._send_json(404, {"error": "not found"})
                    return
                self._send_json(404, {"error": "unexpected path"})

            def do_POST(self):
                if self.headers.get("Authorization") != "Bearer test-token":
                    self._send_json(401, {"error": "unauthorized"})
                    return
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length).decode("utf-8")) if length else {}
                if self.path == "/owner/repo/-/releases":
                    outer.created_payloads.append(body)
                    self._send_json(201, {"id": "rel-99"})
                    return
                if self.path == "/owner/repo/-/releases/rel-99/asset-upload-url":
                    name = body["asset_name"]
                    host = f"http://{self.server.server_address[0]}:{self.server.server_address[1]}"
                    self._send_json(
                        201,
                        {
                            "upload_url": f"{host}/upload/{name}",
                            "verify_url": f"/owner/repo/-/releases/rel-99/asset-upload-confirmation/tok/{name}",
                        },
                    )
                    return
                if self.path.startswith("/owner/repo/-/releases/rel-99/asset-upload-confirmation/tok/"):
                    name = self.path.rsplit("/", 1)[-1]
                    outer.confirmed.append(name)
                    self._send_json(200, {"ok": True})
                    return
                self._send_json(404, {"error": "unexpected post"})

            def do_PUT(self):
                length = int(self.headers.get("Content-Length") or 0)
                data = self.rfile.read(length)
                name = self.path.rsplit("/", 1)[-1]
                if outer.fail_first_upload and not outer._failed_once:
                    outer._failed_once = True
                    self.send_response(502)
                    self.end_headers()
                    return
                outer.upload_order.append(name)
                outer.uploaded_bytes[name] = data
                self.send_response(200)
                self.end_headers()

        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.base_url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.thread.start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


class SyncCNBReleaseTest(unittest.TestCase):
    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="kwp-cnb-sync-test-"))
        self.files = []
        for name, content in [
            ("update-manifest.json", b'{"schemaVersion":1,"version":"1.1.4"}'),
            ("SHA256SUMS.txt", b"abc  KimiWorkPlus-1.1.4-macos-universal.zip\n"),
            ("KimiWorkPlus-1.1.4-macos-universal.zip", b"mac-zip-payload"),
            ("KimiWorkPlus-1.1.4-macos-universal.zip.sig", b"mac-sig\n"),
            ("KimiWorkPlus-1.1.4-windows-amd64.zip", b"win-zip-payload"),
            ("KimiWorkPlus-1.1.4-windows-amd64.zip.sig", b"win-sig\n"),
        ]:
            path = self.tmpdir / name
            path.write_bytes(content)
            self.files.append(path)

    def tearDown(self):
        for item in self.tmpdir.iterdir():
            item.unlink()
        self.tmpdir.rmdir()

    def test_manifest_is_uploaded_last_after_checksums(self):
        server = FakeCNBServer()
        self.addCleanup(server.close)
        release_id, uploaded = sync.sync_release(
            api_base=server.base_url,
            web_base=server.base_url,
            repo="owner/repo",
            token="test-token",
            tag="v1.1.4",
            notes="Release notes",
            files=self.files,
            prerelease=False,
            push_git=False,
        )
        self.assertEqual(release_id, "rel-99")
        self.assertEqual(len(server.created_payloads), 1)
        self.assertEqual(server.created_payloads[0]["make_latest"], "true")
        self.assertEqual(uploaded[-2:], ["SHA256SUMS.txt", "update-manifest.json"])
        self.assertEqual(server.upload_order, uploaded)
        self.assertEqual(server.confirmed, uploaded)
        self.assertEqual(server.uploaded_bytes["update-manifest.json"], b'{"schemaVersion":1,"version":"1.1.4"}')

    def test_existing_release_reused_and_transient_error_retries(self):
        server = FakeCNBServer(existing_release_id="rel-99")
        server.fail_first_upload = True
        self.addCleanup(server.close)
        release_id, uploaded = sync.sync_release(
            api_base=server.base_url,
            web_base=server.base_url,
            repo="owner/repo",
            token="test-token",
            tag="v1.1.4",
            notes="Retry notes",
            files=self.files,
            prerelease=False,
            push_git=False,
        )
        self.assertEqual(release_id, "rel-99")
        self.assertEqual(server.created_payloads, [])
        self.assertEqual(server.confirmed, uploaded)

    def test_prerelease_and_token_resolution(self):
        self.assertFalse(sync.is_prerelease("v1.1.4"))
        self.assertTrue(sync.is_prerelease("v1.1.4-preview.1"))
        tok_path = self.tmpdir / "tok"
        tok_path.write_text("file-tok\n", encoding="utf-8")
        self.assertEqual(sync.resolve_token("env-tok", str(tok_path)), "env-tok")
        self.assertEqual(sync.resolve_token("", str(tok_path)), "file-tok")
        self.assertEqual(sync.resolve_token("", "", default_path=self.tmpdir / "missing"), "")

    def test_stable_main_push_failure_aborts_before_tag_push(self):
        calls = []
        original_run = sync.subprocess.run

        def fake_run(command, **kwargs):
            calls.append(command)
            return sync.subprocess.CompletedProcess(command, 1, stdout="", stderr="main rejected")

        sync.subprocess.run = fake_run
        try:
            with self.assertRaises(RuntimeError) as ctx:
                sync.push_git_refs("https://cnb.example", "owner/repo", "test-token", "v1.1.4", False)
            self.assertIn("main", str(ctx.exception))
            self.assertEqual(len(calls), 1)
        finally:
            sync.subprocess.run = original_run

    def test_untrusted_verify_url_rejected(self):
        server = FakeCNBServer(existing_release_id="rel-99")
        self.addCleanup(server.close)
        orig_request = sync.cnb_json_request

        def fake_request(url, token, method="GET", payload=None, timeout=30):
            if url.endswith("/asset-upload-url"):
                return 201, {
                    "upload_url": f"{server.base_url}/upload/a.bin",
                    "verify_url": "https://evil.example/steal-token",
                }
            return orig_request(url, token, method=method, payload=payload, timeout=timeout)

        sync.cnb_json_request = fake_request
        try:
            with self.assertRaises(RuntimeError) as ctx:
                sync.upload_release_asset(server.base_url, "owner/repo", "test-token", "rel-99", self.files[0], attempts=1)
            self.assertIn("untrusted verify_url", str(ctx.exception))
        finally:
            sync.cnb_json_request = orig_request

    def test_untrusted_upload_url_rejected(self):
        server = FakeCNBServer()
        self.addCleanup(server.close)
        orig_request = sync.cnb_json_request

        def fake_request(url, token, method="GET", payload=None, timeout=30):
            if url.endswith("/asset-upload-url"):
                return 201, {
                    "upload_url": "file:///etc/passwd",
                    "verify_url": f"{server.base_url}/verify/a.bin",
                }
            return orig_request(url, token, method=method, payload=payload, timeout=timeout)

        sync.cnb_json_request = fake_request
        try:
            with self.assertRaises(RuntimeError) as ctx:
                sync.upload_release_asset(server.base_url, "owner/repo", "test-token", "rel-99", self.files[0], attempts=1)
            self.assertIn("untrusted upload_url", str(ctx.exception))
        finally:
            sync.cnb_json_request = orig_request


if __name__ == "__main__":
    unittest.main()
