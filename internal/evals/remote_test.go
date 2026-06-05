package evals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		var req remoteRequest
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
