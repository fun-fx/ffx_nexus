package evals

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOTLPEvaluationEvent_MinimalShape(t *testing.T) {
	s := Score{
		TraceID:   "abc",
		Metric:    "pii",
		Evaluator: "heuristic_pi",
		Score:     1.0,
		Passed:    true,
		Rationale: "no PII detected",
	}
	got := OTLPEvaluationEvent("abc", s)
	if got["name"] != "gen_ai.evaluation" {
		t.Errorf("name = %v", got["name"])
	}
	if got["trace_id"] != "abc" {
		t.Errorf("trace_id = %v", got["trace_id"])
	}
	if got["span_id"] == "" {
		t.Errorf("span_id should be derived from trace_id")
	}
	events, ok := got["events"].([]map[string]any)
	if !ok || len(events) != 1 {
		t.Fatalf("events shape wrong: %+v", got["events"])
	}
	if events[0]["name"] != "gen_ai.evaluation.result" {
		t.Errorf("event name = %v", events[0]["name"])
	}
	attrs, ok := events[0]["attributes"].([]map[string]any)
	if !ok {
		t.Fatalf("attributes shape wrong: %T", events[0]["attributes"])
	}
	wantKeys := map[string]bool{
		"gen_ai.evaluation.score.name":   false,
		"gen_ai.evaluation.score.value":  false,
		"gen_ai.evaluation.score.label":  false,
		"gen_ai.evaluation.explanation":  false,
		"gen_ai.evaluation.evaluator":    false,
	}
	for _, attr := range attrs {
		key, _ := attr["key"].(string)
		if _, ok := wantKeys[key]; ok {
			wantKeys[key] = true
		}
	}
	for k, present := range wantKeys {
		if !present {
			t.Errorf("missing attribute %q", k)
		}
	}
}

func TestOTLPEvaluationEvent_LabelPassVsFail(t *testing.T) {
	pass := Score{TraceID: "t1", Metric: "m", Score: 1, Passed: true}
	fail := Score{TraceID: "t1", Metric: "m", Score: 0, Passed: false}
	if findAttr(t, OTLPEvaluationEvent("t1", pass), "gen_ai.evaluation.score.label") != "pass" {
		t.Error("pass label not 'pass'")
	}
	if findAttr(t, OTLPEvaluationEvent("t1", fail), "gen_ai.evaluation.score.label") != "fail" {
		t.Error("fail label not 'fail'")
	}
}

func TestOTLPEvaluationEvent_FallsBackOnEmptyTraceID(t *testing.T) {
	s := Score{TraceID: "", Metric: "m", Score: 0.5, Passed: true}
	got := OTLPEvaluationEvent("deadbeef", s)
	if got["trace_id"] != "deadbeef" {
		t.Errorf("trace_id fallback = %v", got["trace_id"])
	}
}

func TestOTLPEvaluationEventEnvelope_ResourceShape(t *testing.T) {
	s := Score{TraceID: "abc", Metric: "pii", Score: 1.0, Passed: true}
	env := OTLPEvaluationEventEnvelope("abc", s)
	rs, ok := env["resourceSpans"].([]map[string]any)
	if !ok || len(rs) != 1 {
		t.Fatalf("resourceSpans shape wrong: %+v", env)
	}
	res, ok := rs[0]["resource"].(map[string]any)
	if !ok {
		t.Fatalf("resource shape wrong")
	}
	if !hasAttr(res["attributes"], "service.name") {
		t.Error("service.name missing on resource")
	}
}

func TestOTLPEvaluationEventJSON_RoundTrip(t *testing.T) {
	s := Score{TraceID: "abc", Metric: "pii", Score: 1.0, Passed: true, Rationale: "ok"}
	body, err := OTLPEvaluationEventJSON("abc", s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(body), "gen_ai.evaluation.result") {
		t.Errorf("body missing event name: %s", body)
	}
}

// helpers ------------------------------------------------------------------

func findAttr(t *testing.T, span map[string]any, key string) string {
	t.Helper()
	events, _ := span["events"].([]map[string]any)
	for _, e := range events {
		attrs, _ := e["attributes"].([]map[string]any)
		for _, a := range attrs {
			if a["key"] == key {
				v, _ := a["value"].(map[string]any)
				if s, ok := v["stringValue"].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

func hasAttr(in any, key string) bool {
	attrs, _ := in.([]map[string]any)
	for _, a := range attrs {
		if a["key"] == key {
			return true
		}
	}
	return false
}

var _ = json.Marshal // keep the import hot for helper expansion
