package evals

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// fakeSink captures scores written by the worker.
type fakeSink struct {
	mu     sync.Mutex
	scores []Score
}

func (f *fakeSink) WriteScores(_ context.Context, s []Score) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scores = append(f.scores, s...)
	return nil
}

func (f *fakeSink) all() []Score {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Score, len(f.scores))
	copy(out, f.scores)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Full async path: trace -> Worker -> RemoteEvaluator (HTTP) -> Sink.
func TestWorkerPersistsRemoteScores(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"scores":[{"evaluator":"deepeval","metric":"answer_relevancy","score":0.88,"passed":true}]}`))
	}))
	defer srv.Close()

	sink := &fakeSink{}
	remote := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL})
	w := NewWorker(Options{
		Judges:          []Evaluator{remote},
		Sink:            sink,
		JudgeSampleRate: 1.0,
		Workers:         2,
	}, discardLogger())

	w.Record(observability.Trace{
		TraceID:        "t-ok",
		StatusCode:     200,
		RequestModel:   "gemini-2.5-flash",
		InputMessages:  `[{"role":"user","content":"hi"}]`,
		OutputMessages: "hello",
	})

	_ = w.Close(context.Background())

	got := sink.all()
	if len(got) != 1 || got[0].Metric != "answer_relevancy" || got[0].Score != 0.88 {
		t.Fatalf("expected one remote score persisted, got %+v", got)
	}
}

// Isolation: a dead eval service must not break the worker; heuristic scores
// from other evaluators still persist.
func TestWorkerIsolatesRemoteFailure(t *testing.T) {
	// Closed server => connection refused for the remote evaluator.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	sink := &fakeSink{}
	remote := NewRemoteEvaluator(RemoteConfig{BaseURL: deadURL, Timeout: 200 * time.Millisecond})
	w := NewWorker(Options{
		PIIEnabled:          true,
		CompletenessEnabled: true,
		Judges:              []Evaluator{remote},
		Sink:                sink,
		JudgeSampleRate:     1.0,
		Workers:             2,
	}, discardLogger())

	w.Record(observability.Trace{
		TraceID:        "t-iso",
		StatusCode:     200,
		InputMessages:  `[{"role":"user","content":"email me at a@b.com"}]`,
		OutputMessages: "sure, contact a@b.com",
	})

	_ = w.Close(context.Background())

	got := sink.all()
	if len(got) == 0 {
		t.Fatal("heuristic scores should persist even when remote eval is down")
	}
	for _, s := range got {
		if s.Evaluator == "remote_eval" || s.Evaluator == "deepeval" {
			t.Fatalf("no remote scores expected when service is down, got %+v", s)
		}
	}
}
