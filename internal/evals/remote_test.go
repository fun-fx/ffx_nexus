package evals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
	"github.com/ffxnexus/nexus/internal/observability"
)

func sampleTrace() observability.Trace {
	return observability.Trace{
		TraceID:        "t-1",
		RequestModel:   "gemini-2.5-flash",
		InputMessages:  `[{"role":"user","content":"What is 2+2?"}]`,
		OutputMessages: "4",
	}
}

func TestRemoteEvaluatorDisabledWhenNoURL(t *testing.T) {
	if NewRemoteEvaluator(RemoteConfig{}) != nil {
		t.Fatal("empty BaseURL should disable (nil) the remote evaluator")
	}
}

// TestRemoteEvaluatorEgressClassZeroValueIsTenant is the regression tide-line:
// leaving EgressClass unset MUST preserve the production behaviour.
// Operator class is opt-in only. A change that flips the zero value silently
// to Operator would let a tenant-supplied eval profile ping loopback /
// RFC1918 — exactly the hole the egress guard exists to close.
//
// The internal/evals TestMain relaxes loopback for the package (httptest
// binds 127.0.0.1), so this test must call TestingStrict first to see the
// same policy a production Tenant caller would.
func TestRemoteEvaluatorEgressClassZeroValueIsTenant(t *testing.T) {
	egress.TestingStrict(t)
	// Bind on loopback: Tenant class must reject 127.0.0.1 with the
	// "loopback is not permitted" error. If the zero value drifts to
	// Operator the call below would succeed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"scores":[]}`))
	}))
	defer srv.Close()
	re := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL})
	if re == nil {
		t.Fatal("evaluator should not be nil with a valid BaseURL")
	}
	_, err := re.Evaluate(context.Background(), sampleTrace())
	if err == nil {
		t.Fatal("Tenant class should refuse loopback; nil err here means the " +
			"zero-value EgressClass drifted to Operator and widened tenant reach")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("expected loopback denial, got: %v", err)
	}
}

// TestRemoteEvaluatorEgressClassOperatorReachesLoopback pins the opt-in
// path: a CLI / batch caller that supplies operator-sourced URLs needs
// Operator class to reach localhost in CI. This is the positive-direction
// assertion; the test above is the negative-direction tide-line. Both must
// hold for the field to be safe.
func TestRemoteEvaluatorEgressClassOperatorReachesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"scores":[{"evaluator":"deepeval","metric":"answer_relevancy","score":0.9,"passed":true}]}`))
	}))
	defer srv.Close()
	re := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL, EgressClass: EgressClassOperator})
	if re == nil {
		t.Fatal("evaluator should not be nil with a valid BaseURL")
	}
	scores, err := re.Evaluate(context.Background(), sampleTrace())
	if err != nil {
		t.Fatalf("Operator class should reach loopback, got: %v", err)
	}
	if len(scores) != 1 || scores[0].Metric != "answer_relevancy" {
		t.Fatalf("expected one answer_relevancy score, got %+v", scores)
	}
}

func TestRemoteEvaluatorSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/evaluate" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[
			{"evaluator":"deepeval","metric":"answer_relevancy","score":0.9,"passed":true,"rationale":"relevant","judge_model":"qwen2.5:7b"},
			{"evaluator":"ragas","metric":"faithfulness","score":1.5,"passed":true,"rationale":"clamped"}
		]}`))
	}))
	defer srv.Close()

	r := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL, Metrics: []string{"answer_relevancy"}})
	scores, err := r.Evaluate(context.Background(), sampleTrace())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("want 2 scores, got %d", len(scores))
	}
	if scores[0].Metric != "answer_relevancy" || scores[0].Score != 0.9 {
		t.Fatalf("unexpected first score: %+v", scores[0])
	}
	if scores[1].Score != 1.0 {
		t.Fatalf("score should be clamped to 1.0, got %v", scores[1].Score)
	}
	if scores[0].TraceID != "t-1" {
		t.Fatalf("trace id not propagated: %+v", scores[0])
	}
}

func TestRemoteEvaluatorSkipsEmptyContent(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Write([]byte(`{"scores":[]}`))
	}))
	defer srv.Close()

	r := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL})
	scores, err := r.Evaluate(context.Background(), observability.Trace{TraceID: "x"})
	if err != nil || scores != nil {
		t.Fatalf("expected nil scores/err for empty content, got %v / %v", scores, err)
	}
	if called {
		t.Fatal("service must not be called when content is empty")
	}
}

func TestRemoteEvaluatorServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL})
	if _, err := r.Evaluate(context.Background(), sampleTrace()); err == nil {
		t.Fatal("expected error on 5xx response")
	}
}

func TestRemoteEvaluatorTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{"scores":[]}`))
	}))
	defer srv.Close()

	r := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL, Timeout: 20 * time.Millisecond})
	if _, err := r.Evaluate(context.Background(), sampleTrace()); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRemoteEvaluatorSendsContexts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req RemoteEvaluatorRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Contexts) != 1 || req.Contexts[0] != "Paris is the capital of France." {
			t.Fatalf("contexts not forwarded: %+v", req.Contexts)
		}
		if req.Reference != "Paris" {
			t.Fatalf("reference not forwarded: %q", req.Reference)
		}
		if req.Input != "What is the capital of France?" {
			t.Fatalf("expected extracted prompt, got %q", req.Input)
		}
		if !containsMetric(req.Metrics, "ragas_faithfulness") {
			t.Fatalf("expected RAG metrics in request, got %v", req.Metrics)
		}
		w.Write([]byte(`{"scores":[{"evaluator":"ragas","metric":"ragas_faithfulness","score":0.95,"passed":true}]}`))
	}))
	defer srv.Close()

	tr := observability.Trace{
		TraceID:           "t-rag",
		RequestModel:      "gemini-2.5-flash",
		InputMessages:     `[{"role":"user","content":"What is the capital of France?"}]`,
		OutputMessages:    "Paris",
		RetrievalContexts: `["Paris is the capital of France."]`,
		EvalReference:     "Paris",
	}

	r := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL, Metrics: []string{"answer_relevancy"}})
	scores, err := r.Evaluate(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || scores[0].Metric != "ragas_faithfulness" {
		t.Fatalf("unexpected scores: %+v", scores)
	}
}
