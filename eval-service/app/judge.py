"""Adapters that point DeepEval and RAGAS at an OpenAI-compatible judge.

Heavy libraries are imported lazily so the module (and tests) load without them.
All builders return None when their dependency is unavailable; callers treat a
None judge as "skip this metric".
"""
from __future__ import annotations

import json
import logging
from typing import Any, Optional

from .config import settings

log = logging.getLogger("eval-service.judge")


def build_deepeval_model() -> Optional[Any]:
    """Return a DeepEval custom model backed by the configured judge endpoint."""
    try:
        from deepeval.models import DeepEvalBaseLLM
        from openai import OpenAI
    except Exception as exc:  # pragma: no cover - import guard
        log.warning("deepeval/openai unavailable: %s", exc)
        return None

    class LocalDeepEvalLLM(DeepEvalBaseLLM):
        def __init__(self) -> None:
            self._model = settings.judge_model
            self._client = OpenAI(base_url=settings.judge_base_url, api_key=settings.judge_api_key)

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


def build_ragas_llm() -> Optional[Any]:
    try:
        from langchain_openai import ChatOpenAI
        from ragas.llms import LangchainLLMWrapper
    except Exception as exc:  # pragma: no cover - import guard
        log.warning("ragas/langchain LLM unavailable: %s", exc)
        return None
    chat = ChatOpenAI(
        model=settings.judge_model,
        base_url=settings.judge_base_url,
        api_key=settings.judge_api_key,
        temperature=0,
    )
    return LangchainLLMWrapper(chat)


def build_ragas_embeddings() -> Optional[Any]:
    if not settings.embeddings_enabled:
        return None
    try:
        from langchain_openai import OpenAIEmbeddings
        from ragas.embeddings import LangchainEmbeddingsWrapper
    except Exception as exc:  # pragma: no cover - import guard
        log.warning("ragas/langchain embeddings unavailable: %s", exc)
        return None
    emb = OpenAIEmbeddings(
        model=settings.embeddings_model,
        base_url=settings.embeddings_base_url,
        api_key=settings.embeddings_api_key,
    )
    return LangchainEmbeddingsWrapper(emb)


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
