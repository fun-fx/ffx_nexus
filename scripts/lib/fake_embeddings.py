#!/usr/bin/env python3
"""Minimal OpenAI-compatible /v1/embeddings stub for E2E tests.

Returns a deterministic 8-dim unit vector derived from the input text so
semantically identical prompts produce identical embeddings without a real model.

Usage: python3 fake_embeddings.py <port>
"""
import hashlib
import json
import math
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


def embed(text: str) -> list[float]:
    h = hashlib.sha256(text.encode()).digest()
    raw = [((b / 255.0) * 2 - 1) for b in h[:8]]
    norm = math.sqrt(sum(x * x for x in raw)) or 1.0
    return [x / norm for x in raw]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        if self.path not in ("/v1/embeddings", "/embeddings"):
            self._json(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        text = body.get("input", "")
        if isinstance(text, list):
            text = " ".join(str(t) for t in text)
        vec = embed(text)
        self._json(200, {"data": [{"embedding": vec, "index": 0}]})

    def _json(self, code, payload):
        data = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8300
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
