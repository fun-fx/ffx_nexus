#!/usr/bin/env python3
# py/eval_metric.py — Python side of the heuristic dispatcher.
#
# Protocol: line-delimited JSON, one request per line, one reply
# per line, send/receive on stdin/stdout in this exact order.
#
# Request shape (Go -> Python):
#
#     {"id":"<trace-id>","metric":"hf_evaluate|lighteval|ragas",
#      "name":"<inner metric name>","args":{...},
#      "input":"<prediction text>","reference":"<reference text>"}
#
# Reply shape (Python -> Go):
#
#     {"id":"<trace-id>","value":1.0,"label":"pass","duration_ms":3}
#
# If the metric evaluation raises, return an error reply:
#
#     {"id":"<trace-id>","error":"<message>"}
#
# metric=hf_evaluate + name=any HF Evaluate metric
#     evaluate.load("exact_match").compute(references, predictions)
# metric=lighteval + name=any LightEval task name; default
#     `ifeval` is supported out of the box when installed.
# metric=ragas + name=any Ragas metric; `faithfulness` and
#     `answer_relevancy` are bundled defaults.
#
# Reference: https://huggingface.co/docs/evaluate/v0.4.5/en/index,
# https://docs.ragas.io, https://github.com/huggingface/lighteval.

import json
import sys
import time
import traceback


def evaluate_hf(name, args, prediction, reference):
    # HF Evaluate: evaluate.load(name).compute(references, predictions).
    # The metric class may return a float, dict, or list - normalise
    # the common cases here. If the user's chosen metric returns a
    # non-numeric shape we surface that as an error rather than a 0.
    import evaluate  # lazy import: cold-start cost only applies when
                     # hf metrics are actually requested.
    metric = evaluate.load(name)
    out = metric.compute(references=[reference or ""], predictions=[prediction or ""])
    value = _pick_scalar(out)
    return value, "pass" if value >= float(args.get("threshold", 0.5)) else "fail"


def evaluate_lighteval(name, args, prediction, reference):
    # LightEval: uses the package's high-level `ifeval` / task
    # runner. Older installs export a `lighteval` package; newer ones
    # require `pip install lighteval`. We try both.
    try:
        from lighteval.metrics.ifeval import IFEvalMetric  # type: ignore
    except ImportError as exc:
        raise RuntimeError(
            "lighteval is not installed (`pip install 'lighteval[ifeval]'`) "
            "or import path has changed; check your install"
        ) from exc
    metric = IFEvalMetric()
    score = metric(prediction or "", reference or "")
    value = float(score)
    return value, "pass" if value >= float(args.get("threshold", 0.5)) else "fail"


def evaluate_ragas(name, args, prediction, reference):
    # Ragas: metrics like faithfulness, answer_relevancy,
    # context_precision, etc. require either a `contexts` payload
    # (faithfulness) or a question (answer relevancy). Nexus carries
    # the retrieval context on Trace.RetrievalContexts; this metric
    # reads it through the args["contexts"] override.
    try:
        from ragas.metrics import faithfulness, answer_relevancy  # type: ignore
    except ImportError as exc:
        raise RuntimeError(
            "ragas is not installed (`pip install ragas`); install if you want Ragas metrics"
        ) from exc
    contexts = args.get("contexts", [])
    if name == "faithfulness":
        score = faithfulness.single_turn_score({"answer": prediction, "contexts": contexts})
    else:
        score = answer_relevancy.single_turn_score(
            {"answer": prediction, "question": args.get("question", "")}
        )
    value = float(score)
    return value, "pass" if value >= float(args.get("threshold", 0.5)) else "fail"


def _pick_scalar(out):
    # HF Evaluate metrics may return scalar, dict-of-scalar, or
    # arbitrary nested shapes. Coerce the first number we find.
    if isinstance(out, (int, float)):
        return float(out)
    if isinstance(out, dict):
        for v in out.values():
            try:
                return float(v)
            except (TypeError, ValueError):
                continue
    if isinstance(out, list) and out:
        try:
            return float(out[0])
        except (TypeError, ValueError):
            pass
    raise RuntimeError("metric returned non-scalar shape: %r" % (out,))


_KIND_DISPATCH = {
    "hf_evaluate": evaluate_hf,
    "lighteval": evaluate_lighteval,
    "ragas": evaluate_ragas,
}


def handle(req):
    metric = req.get("metric")
    name = req.get("name")
    args = req.get("args") or {}
    prediction = req.get("input") or ""
    reference = req.get("reference") or ""
    if metric not in _KIND_DISPATCH:
        raise RuntimeError("unknown metric kind %r" % (metric,))
    if not name:
        raise RuntimeError("name is required")
    fn = _KIND_DISPATCH[metric]
    started = time.time()
    value, label = fn(name, args, prediction, reference)
    duration_ms = int((time.time() - started) * 1000)
    return {
        "id": req.get("id"),
        "value": value,
        "label": label,
        "duration_ms": duration_ms,
    }


def main():
    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        try:
            req = json.loads(raw)
            try:
                reply = handle(req)
            except Exception as exc:
                reply = {"id": req.get("id"), "error": str(exc)}
        except Exception as exc:
            reply = {"id": None, "error": "bad request: %s\n%s" % (exc, traceback.format_exc())}
        sys.stdout.write(json.dumps(reply) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
