"""Adapters that point DeepEval and RAGAS at an OpenAI-compatible judge.

Heavy libraries are imported lazily so the module (and tests) load without them.
All builders return None when their dependency is unavailable; callers treat a
None judge as "skip this metric".

PR #136: builders now take an EvaluateRequest and prefer fields on
the request (judge_url / judge_model / threshold) over the env-var
defaults. Falls back to settings.* when a field is omitted so the
sidecar keeps working for callers that send the legacy shape.
"""
from __future__ import annotations

import json
import logging
from typing import Any, Optional

from .config import settings
from .schemas import EvaluateRequest

log = logging.getLogger("eval-service.judge")


def _judge_url(req: EvaluateRequest) -> str:
    return req.judge_url or settings.judge_base_url


def _judge_model(req: EvaluateRequest) -> str:
    return req.judge_model or settings.judge_model


def _judge_api_key(req: EvaluateRequest) -> str:
    # Settings carry a default placeholder like "not-needed" so Ollama
    # / vLLM work without auth. The override path passes a real bearer
    # token when the profile is user-scoped (BYOK).
    return settings.judge_api_key


def _embeddings_url(req: EvaluateRequest) -> str:
    return settings.embeddings_base_url


def _embeddings_model(req: EvaluateRequest) -> str:
    return settings.embeddings_model


def _embeddings_api_key(req: EvaluateRequest) -> str:
    return settings.embeddings_api_key


def _threshold(req: EvaluateRequest) -> float:
    if req.threshold is not None and req.threshold > 0:
        return float(req.threshold)
    return settings.threshold


def build_deepeval_model(req: EvaluateRequest) -> Optional[Any]:
    """Return a DeepEval custom model backed by the configured judge endpoint.

    Override precedence: req.judge_url > settings.judge_base_url.
    """
    try:
        from deepeval.models import DeepEvalBaseLLM
        from openai import OpenAI
    except Exception as exc:  # pragma: no cover - import guard
        log.warning("deepeval/openai unavailable: %s", exc)
        return None

    model_name = _judge_model(req)
    base_url = _judge_url(req)
    api_key = _judge_api_key(req)

    class LocalDeepEvalLLM(DeepEvalBaseLLM):
        def __init__(self) -> None:
            self._model = model_name
            self._client = OpenAI(base_url=base_url, api_key=api_key)

        def load_model(self):
            return self._client

        def _complete(self, prompt: str) -> str:
            resp = self._client.chat.completions.create(
                model=self._model,
                messages=[{"role": "user", "content": prompt}],
                temperature=0,
            )
            return resp.choices[0].message.content or ""

        def generate(self, prompt: str, schema: Any | None = None) -> Any:
            text = self._complete(prompt)
            if schema is not None:
                return _coerce_schema(text, schema)
            return text

        async def a_generate(self, prompt: str, schema: Any | None = None) -> Any:
            return self.generate(prompt, schema)

        def get_model_name(self) -> str:
            return self._model

    return LocalDeepEvalLLM()


def build_ragas_llm(req: EvaluateRequest) -> Optional[Any]:
    try:
        from langchain_openai import ChatOpenAI
        from ragas.llms import LangchainLLMWrapper
    except Exception as exc:  # pragma: no cover - import guard
        log.warning("ragas/langchain LLM unavailable: %s", exc)
        return None
    chat = ChatOpenAI(
        model=_judge_model(req),
        base_url=_judge_url(req),
        api_key=_judge_api_key(req),
        temperature=0,
    )
    return LangchainLLMWrapper(chat)


def build_ragas_embeddings(req: EvaluateRequest) -> Optional[Any]:
    if not _embeddings_url(req):
        return None
    try:
        from langchain_openai import OpenAIEmbeddings
        from ragas.embeddings import LangchainEmbeddingsWrapper
    except Exception as exc:  # pragma: no cover - import guard
        log.warning("ragas/langchain embeddings unavailable: %s", exc)
        return None
    emb = OpenAIEmbeddings(
        model=_embeddings_model(req),
        base_url=_embeddings_url(req),
        api_key=_embeddings_api_key(req),
    )
    return LangchainEmbeddingsWrapper(emb)


def threshold_for(req: EvaluateRequest) -> float:
    return _threshold(req)


_PASS_DIRECTION_HIGHER = "higher"
_PASS_DIRECTION_LOWER = "lower"


def passed(req: EvaluateRequest, score: float, higher_is_better: bool) -> bool:
    t = _threshold(req)
    return score >= t if higher_is_better else score <= t


def _coerce_schema(text: str, schema: Any) -> Any:
    """Best-effort: parse JSON out of a model response into a pydantic schema."""
    start, end = text.find("{"), text.rfind("}")
    if start != -1 and end != -1 and end > start:
        text = text[start : end + 1]
    data = json.loads(text)
    try:
        return schema(**data)  # pydantic v1/v2 constructor
    except TypeError:
        return schema.model_validate(data)
