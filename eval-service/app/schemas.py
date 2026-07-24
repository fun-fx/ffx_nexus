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


# Per-request overrides. PR #136: when set they take precedence over
# the operator's NEXUS_JUDGE_BASE_URL / NEXUS_EMBEDDINGS_BASE_URL
# env vars. Optional in production by design — the sidecar still
# works without them and falls back to the env-var defaults.
    judge_url: str | None = None
    judge_model: str | None = None
    # Override the pass/fail threshold for this trace only. Empty keeps
    # the sidecar's default (METRIC_THRESHOLD env var).
    threshold: float | None = None


class EvalBatchRequest(BaseModel):
    """PR #136 batch contract: N traces per sidecar call.

    Field-per-trace shape mirrors EvaluateRequest so the sidecar can
    iterate over items without re-deriving the context.
    """
    items: list[EvaluateRequest] = Field(default_factory=list)


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


class EvalBatchResponse(BaseModel):
    """PR #136 batch reply. scores_by_trace keeps the trace_id → ScoreOut
    association intact even when traces re-order on the sidecar."""
    scores_by_trace: dict[str, list[ScoreOut]] = Field(default_factory=dict)
