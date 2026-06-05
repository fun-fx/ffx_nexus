"""Nexus eval-service: async Python sidecar running DeepEval + RAGAS.

The Go gateway never calls this service on the request hot path. Its eval
worker invokes it out-of-band on sampled traces, so any latency or failure here
degrades gracefully to the Go heuristics without affecting client responses.
"""

__version__ = "0.1.0"
