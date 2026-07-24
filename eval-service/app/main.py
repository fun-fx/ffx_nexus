"""FastAPI entrypoint for the Nexus eval-service.

PR #136 widens the surface: /evaluate now honours per-request judge_url
/ judge_model / threshold overrides; /evaluate/batch accepts a list of
EvaluateRequest items in one HTTP call so the Go Worker can amortise
TCP/TLS + DeepEval cold-start across a window of traces.
"""
from __future__ import annotations

import asyncio
import logging
from typing import Tuple

from fastapi import FastAPI

from . import __version__
from .config import settings
from .metrics import REGISTRY, run_metrics
from .schemas import (
    EvalBatchRequest,
    EvalBatchResponse,
    EvaluateRequest,
    EvaluateResponse,
    ScoreOut,
)

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")

app = FastAPI(title="nexus-eval-service", version=__version__)


@app.get("/healthz")
async def healthz() -> dict:
    return {
        "status": "ok",
        "version": __version__,
        "judge_model": settings.judge_model,
        "embeddings_enabled": settings.embeddings_enabled,
        "metrics": sorted(REGISTRY.keys()),
        "default_metrics": settings.default_metrics,
    }


@app.post("/evaluate", response_model=EvaluateResponse)
async def evaluate(req: EvaluateRequest) -> EvaluateResponse:
    scores, skipped = await run_metrics(req)
    return EvaluateResponse(scores=scores, skipped=skipped)


@app.post("/evaluate/batch", response_model=EvalBatchResponse)
async def evaluate_batch(req: EvalBatchRequest) -> EvalBatchResponse:
    """PR #136 batch handler.

    Iterates the requested items concurrently. Errors on a single item
    stay localised — the Go Worker treats a batch with one failure as
    scoring failures for that trace and continues with the rest.
    """
    if not req.items:
        return EvalBatchResponse()

    async def _one(item: EvaluateRequest) -> Tuple[str, list[ScoreOut]]:
        try:
            scores, _skipped = await run_metrics(item)
            return item.trace_id, scores
        except Exception as exc:  # pragma: no cover - per-item isolation
            log.warning("item %s failed: %s", item.trace_id, exc)
            return item.trace_id, []

    tasks = [_one(item) for item in req.items]
    results = await asyncio.gather(*tasks)
    return EvalBatchResponse(scores_by_trace={tid: scores for tid, scores in results})
