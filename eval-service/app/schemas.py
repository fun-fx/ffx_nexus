"""Request/response contract shared with the Go RemoteEvaluator.

Field names must stay in sync with internal/evals/remote.go.
"""
from __future__ import annotations

from pydantic import BaseModel, Field


class EvaluateRequest(BaseModel):
    trace_id: str = ""
    model: str = ""
    input: str = ""
    output: str = ""
    # Optional RAG inputs. When absent, context-dependent metrics are skipped.
    contexts: list[str] = Field(default_factory=list)
    reference: str | None = None
    # Requested metric ids. Empty => server default_metrics.
    metrics: list[str] = Field(default_factory=list)


class ScoreOut(BaseModel):
    evaluator: str
    metric: str
    score: float
    passed: bool
    rationale: str = ""
    judge_model: str = ""


class SkippedMetric(BaseModel):
    metric: str
    reason: str


class EvaluateResponse(BaseModel):
    scores: list[ScoreOut] = Field(default_factory=list)
    skipped: list[SkippedMetric] = Field(default_factory=list)
