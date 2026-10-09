#!/usr/bin/env python3
"""Serve only the runner's inference ServiceAccount assertion to fullsend run."""

import hashlib
import http.server
import json
import os
import pathlib
import time

TOKEN_FILE = "/var/run/fullsend/wif/token"
AUTH_FILE = "/tmp/wif-auth"
METRICS_FILE = os.environ["WIF_REFRESH_METRICS_FILE"]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802
        if self.path == "/health":
            self.send_response(204)
            self.end_headers()
            return
        expected = pathlib.Path(AUTH_FILE).read_text().strip()
        if self.path != "/token" or self.headers.get("Authorization") != expected:
            self.send_error(403)
            return
        assertion = pathlib.Path(TOKEN_FILE).read_text().strip()
        if not assertion:
            self.send_error(503)
            return
        digest = hashlib.sha256(assertion.encode()).hexdigest()
        metrics_path = pathlib.Path(METRICS_FILE)
        try:
            metrics = json.loads(metrics_path.read_text())
        except FileNotFoundError:
            metrics = {"requests": []}
        metrics["requests"].append({"at_unix": int(time.time()), "assertion_sha256": digest})
        temporary = metrics_path.with_suffix(".tmp")
        temporary.write_text(json.dumps(metrics, separators=(",", ":")))
        os.chmod(temporary, 0o600)
        os.replace(temporary, metrics_path)
        body = json.dumps({"value": assertion}, separators=(",", ":")).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        return None


http.server.ThreadingHTTPServer(("127.0.0.1", 18765), Handler).serve_forever()
