"""API contract tests using FastAPI's TestClient."""
from fastapi.testclient import TestClient

from app import metrics
from app.main import app
from app.metrics import MetricSpec
from app.schemas import ScoreOut

client = TestClient(app)


def test_healthz():
    r = client.get("/healthz")
    assert r.status_code == 200
    body = r.json()
    assert body["status"] == "ok"
    assert "answer_relevancy" in body["metrics"]


def test_evaluate_returns_scores(monkeypatch):
    def run(req):
        return ScoreOut(evaluator="fake", metric="answer_relevancy", score=0.95, passed=True)

    monkeypatch.setattr(
        metrics,
        "REGISTRY",
        {"answer_relevancy": MetricSpec("answer_relevancy", "fake", False, True, run)},
    )
    monkeypatch.setattr(metrics.settings, "default_metrics", ["answer_relevancy"])

    r = client.post("/evaluate", json={"trace_id": "t-1", "input": "2+2?", "output": "4"})
    assert r.status_code == 200
    body = r.json()
    assert len(body["scores"]) == 1
    assert body["scores"][0]["metric"] == "answer_relevancy"
    assert body["scores"][0]["score"] == 0.95


def test_evaluate_empty_request_ok(monkeypatch):
    monkeypatch.setattr(metrics.settings, "default_metrics", [])
    r = client.post("/evaluate", json={})
    assert r.status_code == 200
    assert r.json()["scores"] == []
