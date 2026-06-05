"""Metric registry wrapping DeepEval and RAGAS.

Each metric is registered with the inputs it needs. The runner executes the
requested metrics concurrently (in worker threads, since the libraries are
blocking) and skips — rather than fails — any metric whose dependency is
missing, whose required inputs are absent, or that raises at runtime. The
service therefore always returns 200 with whatever succeeded.

Score direction (higher_is_better) is documented per metric so the Go side and
dashboards interpret `score`/`passed` correctly.
"""
from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass
from typing import Callable, Optional

from .config import settings
from .judge import build_deepeval_model, build_ragas_embeddings, build_ragas_llm
from .schemas import EvaluateRequest, ScoreOut, SkippedMetric

log = logging.getLogger("eval-service.metrics")


@dataclass
class MetricSpec:
    metric_id: str
    evaluator: str  # "deepeval" | "ragas"
    needs_contexts: bool
    higher_is_better: bool
    run: Callable[[EvaluateRequest], ScoreOut]


def _passed(score: float, higher_is_better: bool) -> bool:
    return score >= settings.threshold if higher_is_better else score <= settings.threshold


# --- DeepEval metrics -------------------------------------------------------

def _deepeval_case(req: EvaluateRequest):
    from deepeval.test_case import LLMTestCase

    return LLMTestCase(
        input=req.input,
        actual_output=req.output,
        retrieval_context=req.contexts or None,
        context=req.contexts or None,
        expected_output=req.reference,
    )


def _run_deepeval(metric_cls_path: str, metric_id: str, higher_is_better: bool):
    def runner(req: EvaluateRequest) -> ScoreOut:
        module_name, cls_name = metric_cls_path.rsplit(".", 1)
        mod = __import__(module_name, fromlist=[cls_name])
        metric_cls = getattr(mod, cls_name)
        model = build_deepeval_model()
        metric = metric_cls(model=model, threshold=settings.threshold)
        metric.measure(_deepeval_case(req))
        score = float(metric.score if metric.score is not None else 0.0)
        return ScoreOut(
            evaluator="deepeval",
            metric=metric_id,
            score=score,
            passed=_passed(score, higher_is_better),
            rationale=(getattr(metric, "reason", "") or "")[:1000],
            judge_model=settings.judge_model,
        )

    return runner


# --- RAGAS metrics ----------------------------------------------------------

def _run_ragas(metric_factory: str, metric_id: str, higher_is_better: bool):
    def runner(req: EvaluateRequest) -> ScoreOut:
        from datasets import Dataset
        from ragas import evaluate as ragas_evaluate

        module_name, attr = metric_factory.rsplit(".", 1)
        mod = __import__(module_name, fromlist=[attr])
        metric_obj = getattr(mod, attr)

        llm = build_ragas_llm()
        embeddings = build_ragas_embeddings()
        ds = Dataset.from_dict(
            {
                "question": [req.input],
                "answer": [req.output],
                "contexts": [req.contexts],
                "ground_truth": [req.reference or ""],
            }
        )
        result = ragas_evaluate(
            ds,
            metrics=[metric_obj],
            llm=llm,
            embeddings=embeddings,
            raise_exceptions=True,
        )
        df = result.to_pandas()
        col = [c for c in df.columns if c not in ("question", "answer", "contexts", "ground_truth")][0]
        score = float(df[col].iloc[0])
        return ScoreOut(
            evaluator="ragas",
            metric=metric_id,
            score=score,
            passed=_passed(score, higher_is_better),
            rationale="",
            judge_model=settings.judge_model,
        )

    return runner


REGISTRY: dict[str, MetricSpec] = {
    "answer_relevancy": MetricSpec(
        "answer_relevancy", "deepeval", needs_contexts=False, higher_is_better=True,
        run=_run_deepeval("deepeval.metrics.AnswerRelevancyMetric", "answer_relevancy", True),
    ),
    "toxicity": MetricSpec(
        "toxicity", "deepeval", needs_contexts=False, higher_is_better=False,
        run=_run_deepeval("deepeval.metrics.ToxicityMetric", "toxicity", False),
    ),
    "bias": MetricSpec(
        "bias", "deepeval", needs_contexts=False, higher_is_better=False,
        run=_run_deepeval("deepeval.metrics.BiasMetric", "bias", False),
    ),
    "hallucination": MetricSpec(
        "hallucination", "deepeval", needs_contexts=True, higher_is_better=False,
        run=_run_deepeval("deepeval.metrics.HallucinationMetric", "hallucination", False),
    ),
    "ragas_faithfulness": MetricSpec(
        "ragas_faithfulness", "ragas", needs_contexts=True, higher_is_better=True,
        run=_run_ragas("ragas.metrics.faithfulness", "ragas_faithfulness", True),
    ),
    "ragas_answer_relevancy": MetricSpec(
        "ragas_answer_relevancy", "ragas", needs_contexts=False, higher_is_better=True,
        run=_run_ragas("ragas.metrics.answer_relevancy", "ragas_answer_relevancy", True),
    ),
}


async def run_metrics(req: EvaluateRequest) -> tuple[list[ScoreOut], list[SkippedMetric]]:
    requested = req.metrics or settings.default_metrics
    scores: list[ScoreOut] = []
    skipped: list[SkippedMetric] = []

    async def _one(metric_id: str) -> Optional[ScoreOut]:
        spec = REGISTRY.get(metric_id)
        if spec is None:
            skipped.append(SkippedMetric(metric=metric_id, reason="unknown metric"))
            return None
        if spec.needs_contexts and not req.contexts:
            skipped.append(SkippedMetric(metric=metric_id, reason="requires contexts (none provided)"))
            return None
        try:
            return await asyncio.to_thread(spec.run, req)
        except Exception as exc:  # graceful per-metric isolation
            log.warning("metric %s failed: %s", metric_id, exc)
            skipped.append(SkippedMetric(metric=metric_id, reason=str(exc)[:200]))
            return None

    results = await asyncio.gather(*[_one(m) for m in requested])
    scores = [r for r in results if r is not None]
    return scores, skipped
