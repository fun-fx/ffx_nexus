#!/usr/bin/env python3
"""Minimal stand-in for the Python eval-service, used by E2E tests.

Implements the same HTTP contract as eval-service/app (POST /evaluate,
GET /healthz) without DeepEval/RAGAS or an LLM, so CI can verify the Go
gateway <-> eval-service wiring deterministically and with zero heavy deps.

Usage: python3 fake_eval_service.py <port>
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence default logging
        pass

    def do_GET(self):
        if self.path == "/healthz":
            self._json(200, {"status": "ok", "metrics": ["answer_relevancy", "toxicity", "bias"]})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/evaluate":
            self._json(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        metrics = body.get("metrics") or ["answer_relevancy", "toxicity", "bias"]
        contexts = body.get("contexts") or []
        scores = []
        for m in metrics:
            score = 0.9 if m == "answer_relevancy" else 0.05
            if m in ("ragas_faithfulness", "hallucination") and contexts:
                score = 0.88
            scores.append({
                "evaluator": "deepeval" if m != "ragas_faithfulness" else "ragas",
                "metric": m,
                "score": score,
                "passed": True,
                "rationale": f"stub score for {m}",
                "judge_model": "fake-judge",
            })
        self._json(200, {"scores": scores, "skipped": []})

    def _json(self, code, payload):
        data = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8200
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
