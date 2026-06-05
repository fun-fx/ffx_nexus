"""Runtime configuration, sourced from environment variables.

Kept dependency-free (plain os.getenv) so importing the config never pulls in
the heavy eval libraries.
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field


def _csv(value: str) -> list[str]:
    return [p.strip() for p in value.split(",") if p.strip()]


@dataclass
class Settings:
    # Judge LLM: any OpenAI-compatible endpoint (Ollama, vLLM, OpenAI, ...).
    # Reuses the same local judge as the Go SLM judge by default.
    judge_base_url: str = field(default_factory=lambda: os.getenv("JUDGE_BASE_URL", "http://localhost:11434/v1"))
    judge_model: str = field(default_factory=lambda: os.getenv("JUDGE_MODEL", "qwen2.5:7b-instruct"))
    judge_api_key: str = field(default_factory=lambda: os.getenv("JUDGE_API_KEY", "not-needed"))

    # Embeddings endpoint (optional, only required for RAGAS metrics).
    embeddings_base_url: str = field(default_factory=lambda: os.getenv("EMBEDDINGS_BASE_URL", ""))
    embeddings_model: str = field(default_factory=lambda: os.getenv("EMBEDDINGS_MODEL", "nomic-embed-text"))
    embeddings_api_key: str = field(default_factory=lambda: os.getenv("EMBEDDINGS_API_KEY", "not-needed"))

    default_metrics: list[str] = field(
        default_factory=lambda: _csv(os.getenv("DEFAULT_METRICS", "answer_relevancy,toxicity,bias"))
    )
    # Threshold below/above which a metric is considered "passed". Direction is
    # metric-specific and documented in metrics.py.
    threshold: float = field(default_factory=lambda: float(os.getenv("METRIC_THRESHOLD", "0.5")))

    @property
    def embeddings_enabled(self) -> bool:
        return bool(self.embeddings_base_url)


settings = Settings()
