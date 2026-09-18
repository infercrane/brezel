from __future__ import annotations

import base64
import email.message
import json
import os
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parents[1] / "src"))

from brezel import BrezelClient, BrezelError, Sandbox  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    requests: list[tuple[str, str, dict[str, str], bytes]] = []

    def log_message(self, format: str, *args: object) -> None:
        return

    def _record(self) -> bytes:
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        self.requests.append((self.command, self.path, dict(self.headers), body))
        return body

    def _json(self, status: int, body: object) -> None:
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self) -> None:
        self._record()
        if self.path == "/v1/environments":
            self._json(202, {"resource": {"revision_id": "envr_test"}, "operation": {"state": "succeeded"}})
        elif self.path == "/v1/sandboxes":
            self._json(202, {"resource": {"id": "sbx_test", "state": "running"}, "operation": {"state": "succeeded"}})
        elif self.path == "/v1/sandboxes/sbx_test/commands":
            events = [
                {"execution_id": "exec_test", "type": "stdout", "data": base64.b64encode(b"42\n").decode()},
                {"execution_id": "exec_test", "type": "stderr", "data": base64.b64encode(b"note\n").decode()},
                {"execution_id": "exec_test", "type": "exited", "exited": True, "exit_code": 0},
            ]
            body = b"".join(json.dumps(event).encode() + b"\n" for event in events)
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/v1/sandboxes/sbx_test/ports/3000/leases":
            self._json(201, {"path": "/p/opaque/", "expires_at": "2026-09-17T00:00:00Z"})
        elif self.path.endswith(":pause") or self.path.endswith(":resume"):
            state = "standby" if self.path.endswith(":pause") else "running"
            self._json(202, {"resource": {"id": "sbx_test", "state": state}, "operation": {"state": "succeeded"}})
        else:
            self._json(404, {"error": {"code": "not_found", "message": "resource not found"}})

    def do_PUT(self) -> None:
        body = self._record()
        self._json(200, {"path": "/workspace/value.txt", "size": len(body)})

    def do_GET(self) -> None:
        self._record()
        if self.path.startswith("/v1/sandboxes/sbx_test/files?"):
            body = b"stored"
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/v1/sandboxes":
            self._json(200, {"sandboxes": [{"id": "sbx_test", "state": "running"}]})
        else:
            self._json(404, {"error": {"code": "not_found", "message": "resource not found"}})

    def do_DELETE(self) -> None:
        self._record()
        self._json(202, {"resource": {"id": "sbx_test", "state": "deleted"}, "operation": {"state": "succeeded"}})


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.base_url = f"http://127.0.0.1:{cls.server.server_port}"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self) -> None:
        Handler.requests.clear()
        self.client = BrezelClient(token="a-service-token-that-is-long-enough", base_url=self.base_url, project="project-a")

    def test_supported_lifecycle(self) -> None:
        with self.client.create_sandbox(template="base", ttl_seconds=900) as sandbox:
            result = sandbox.run(["python3", "-c", "print(6 * 7)"], check=True)
            self.assertEqual(result.stdout, b"42\n")
            self.assertEqual(result.stderr, b"note\n")
            self.assertEqual(result.exit_code, 0)
            self.assertEqual(sandbox.write_file("/workspace/value.txt", b"stored")["size"], 6)
            self.assertEqual(sandbox.read_file("/workspace/value.txt"), b"stored")
            self.assertEqual(sandbox.preview(3000), self.base_url + "/p/opaque/")
            self.assertEqual(sandbox.pause()["state"], "standby")
            self.assertEqual(sandbox.resume()["state"], "running")
        self.assertTrue(any(method == "DELETE" for method, _, _, _ in Handler.requests))
        for _, _, headers, _ in Handler.requests:
            self.assertEqual(headers.get("Authorization"), "Bearer a-service-token-that-is-long-enough")
            self.assertEqual(headers.get("X-Project-Id"), "project-a")

    def test_private_token_file_and_remote_http_guard(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "token"
            path.write_text("a-service-token-that-is-long-enough\n")
            path.chmod(0o600)
            client = BrezelClient.from_token_file(path, base_url=self.base_url)
            self.assertEqual(client.token, "a-service-token-that-is-long-enough")
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                BrezelClient.from_token_file(path, base_url=self.base_url)
        with self.assertRaises(ValueError):
            BrezelClient(token="a-service-token-that-is-long-enough", base_url="http://example.com")

    def test_immutable_environment_revision_skips_environment_mutation(self) -> None:
        sandbox = self.client.create_sandbox(environment_revision="envr_exact")
        try:
            environment_requests = [request for request in Handler.requests if request[1] == "/v1/environments"]
            self.assertEqual(environment_requests, [])
            create = next(
                request for request in Handler.requests
                if request[0] == "POST" and request[1] == "/v1/sandboxes"
            )
            self.assertEqual(json.loads(create[3])["environment_revision"], "envr_exact")
            with self.assertRaisesRegex(ValueError, "mutually exclusive"):
                self.client.create_sandbox(template="base", environment_revision="envr_exact")
        finally:
            sandbox.delete()

    def test_indeterminate_stream_fails_closed(self) -> None:
        original = self.client._open

        class Truncated:
            lines = iter([json.dumps({"type": "stdout", "data": base64.b64encode(b"partial").decode()}).encode() + b"\n"])
            headers = email.message.Message()
            headers["Content-Type"] = "application/x-ndjson"

            def readline(self, maximum: int) -> bytes:
                return next(self.lines, b"")

            def close(self) -> None:
                return

        self.client._open = lambda *args, **kwargs: Truncated()  # type: ignore[method-assign]
        try:
            with self.assertRaisesRegex(BrezelError, "confirmed exit"):
                Sandbox(self.client, "sbx_test").run(["true"])
        finally:
            self.client._open = original  # type: ignore[method-assign]


if __name__ == "__main__":
    unittest.main()
