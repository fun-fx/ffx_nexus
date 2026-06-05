"""Unit tests for the metric runner: routing, graceful skip, and isolation.

The real DeepEval/RAGAS engines are replaced with fakes so the tests run with
no LLM, no GPU, and only the lightweight test deps installed.
"""
import pytest

from app import metrics
from app.metrics import MetricSpec, run_metrics
from app.schemas import EvaluateRequest, ScoreOut


def _fake_spec(metric_id, *, needs_contexts=False, higher=True, raises=False, score=0.8):
    def run(req):
        if raises:
            raise RuntimeError("judge exploded")
        return ScoreOut(evaluator="fake", metric=metric_id, score=score, passed=True)

    return MetricSpec(metric_id, "fake", needs_contexts, higher, run)


@pytest.fixture
def registry(monkeypatch):
    reg = {
        "answer_relevancy": _fake_spec("answer_relevancy"),
        "toxicity": _fake_spec("toxicity", higher=False, score=0.1),
        "faithfulness": _fake_spec("faithfulness", needs_contexts=True),
        "explodes": _fake_spec("explodes", raises=True),
    }
    monkeypatch.setattr(metrics, "REGISTRY", reg)
    return reg


@pytest.fixture
def default_metrics(monkeypatch):
    monkeypatch.setattr(metrics.settings, "default_metrics", ["answer_relevancy", "toxicity"])


async def test_default_metrics_used_when_unspecified(registry, default_metrics):
    scores, skipped = await run_metrics(EvaluateRequest(input="q", output="a"))
    got = sorted(s.metric for s in scores)
    assert got == ["answer_relevancy", "toxicity"]
    assert skipped == []


async def test_unknown_metric_skipped(registry):
    scores, skipped = await run_metrics(EvaluateRequest(input="q", output="a", metrics=["nope"]))
    assert scores == []
    assert skipped[0].metric == "nope"
    assert "unknown" in skipped[0].reason


async def test_context_metric_skipped_without_contexts(registry):
    scores, skipped = await run_metrics(EvaluateRequest(input="q", output="a", metrics=["faithfulness"]))
    assert scores == []
    assert skipped[0].metric == "faithfulness"
    assert "contexts" in skipped[0].reason


async def test_context_metric_runs_with_contexts(registry):
    req = EvaluateRequest(input="q", output="a", metrics=["faithfulness"], contexts=["doc"])
    scores, skipped = await run_metrics(req)
    assert [s.metric for s in scores] == ["faithfulness"]
    assert skipped == []


async def test_failing_metric_isolated(registry):
    req = EvaluateRequest(input="q", output="a", metrics=["answer_relevancy", "explodes"])
    scores, skipped = await run_metrics(req)
    assert [s.metric for s in scores] == ["answer_relevancy"]
    assert skipped[0].metric == "explodes"
    assert "exploded" in skipped[0].reason
