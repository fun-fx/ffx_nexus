"""FastAPI entrypoint for the Nexus eval-service."""
from __future__ import annotations

import logging

from fastapi import FastAPI

from . import __version__
from .config import settings
from .metrics import REGISTRY, run_metrics
from .schemas import EvaluateRequest, EvaluateResponse

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
