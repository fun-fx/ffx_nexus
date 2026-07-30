package external

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// fakeLocal is the controllable LocalEvaluator used by these tests
// so we don't have to depend on the heuristic package from here.
type fakeLocal struct {
	calls    int
	metricIn string
	argsSeen map[string]any
	err      error
	scores   []evals.Score
}

func (f *fakeLocal) Evaluate(_ context.Context, metricName string, args map[string]any, _ observability.Trace) ([]evals.Score, error) {
	f.calls++
	f.metricIn = metricName
	f.argsSeen = args
	if f.err != nil {
		return nil, f.err
	}
	return f.scores, nil
}

// recordingLogger captures log lines so tests can assert without
// depending on log/slog internals.
type recordingLogger struct {
	text string
	lg   *slog.Logger
}

func (r *recordingLogger) Write(p []byte) (int, error) {
	r.text += string(p)
	return len(p), nil
}

func newRecordingLogger() *recordingLogger {
	r := &recordingLogger{}
	r.lg = slog.New(slog.NewTextHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return r
}

func TestMultiEvaluator_RoutesHeuristicToLocalEvaluator(t *testing.T) {
	reg := evalplugin.NewRegistry()
	reg.Merge([]evalplugin.Record{makeHeuristicRecord("pii-check", "pii")})

	m := NewMultiEvaluator(reg, nil)
	fake := &fakeLocal{
		scores: []evals.Score{{TraceID: "trace-x", Metric: "pii", Score: 1.0, Passed: true}},
	}
	m.SetLocalEvaluator(fake)

	out, err := m.Evaluate(context.Background(), observability.Trace{
		TraceID: "trace-x",
		OrgID:   "",
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("local calls = %d want 1", fake.calls)
	}
	if fake.metricIn != "pii" {
		t.Errorf("metric = %q want pii", fake.metricIn)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 score, got %+v", out)
	}
	if out[0].TraceID != "trace-x" {
		t.Errorf("score trace id mismatch: %+v", out[0])
	}
}

func TestMultiEvaluator_HeuristicWithoutLocalLogsAndSkips(t *testing.T) {
	rec := newRecordingLogger()
	reg := evalplugin.NewRegistry()
	reg.Merge([]evalplugin.Record{makeHeuristicRecord("pii-check", "pii")})

	m := NewMultiEvaluator(reg, nil)
	m.SetLogger(rec.lg)
	out, err := m.Evaluate(context.Background(), observability.Trace{
		TraceID: "trace-y",
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected no scores when local evaluator not wired, got %+v", out)
	}
	if !strings.Contains(rec.text, "LocalEvaluator not wired") {
		t.Errorf("expected warning, got %q", rec.text)
	}
}

func TestMultiEvaluator_HeuristicErrorIsVisible(t *testing.T) {
	rec := newRecordingLogger()
	reg := evalplugin.NewRegistry()
	reg.Merge([]evalplugin.Record{makeHeuristicRecord("contains-check", "contains")})

	m := NewMultiEvaluator(reg, nil)
	m.SetLocalEvaluator(&fakeLocal{err: errors.New("metric args invalid")})
	m.SetLogger(rec.lg)

	out, err := m.Evaluate(context.Background(), observability.Trace{TraceID: "trace-z"})
	if err != nil {
		t.Fatalf("Evaluate returned err (must not): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("errors must not produce a score: %+v", out)
	}
	if !strings.Contains(rec.text, "heuristic plugin failed") {
		t.Errorf("expected heuristic plugin failed log, got %q", rec.text)
	}
}

func TestMultiEvaluator_NonHeuristicDoesNotConsumeLocal(t *testing.T) {
	reg := evalplugin.NewRegistry()
	// A plugin of type langsmith — not heuristic — must NOT call the
	// local evaluator.
	reg.Merge([]evalplugin.Record{makeVendorRecord("langsmith-judge")})

	m := NewMultiEvaluator(reg, nil)
	fake := &fakeLocal{}
	m.SetLocalEvaluator(fake)

	// Dispatcher is nil so the dispatch path panics; we don't care
	// about the panic — what matters is that fake.calls stays at 0.
	defer func() {
		_ = recover()
		if fake.calls != 0 {
			t.Errorf("non-heuristic plugin should not consume LocalEvaluator; calls=%d", fake.calls)
		}
	}()
	_, _ = m.Evaluate(context.Background(), observability.Trace{TraceID: "trace-v"})
}

// Helpers --------------------------------------------------------------------

func makeHeuristicRecord(name, metric string) evalplugin.Record {
	return evalplugin.Record{
		Plugin: &evalplugin.Plugin{
			APIVersion: evalplugin.PluginAPIVersionV1Alpha2,
			Kind:       evalplugin.PluginKind,
			Metadata:   evalplugin.PluginMetadata{Name: name},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{
					Type:   evalplugin.ServiceHeuristic,
					Metric: evalplugin.MetricSpec{Name: metric},
				},
				Send: evalplugin.SendSpec{
					Trigger:  "on_trace",
					Sampling: evalplugin.SamplingFraction(1.0),
				},
				Collect: evalplugin.CollectSpec{Mode: "sync"},
			},
		},
		Source:  evalplugin.Source{Kind: evalplugin.SourceHelm},
		Enabled: true,
	}
}

func makeVendorRecord(name string) evalplugin.Record {
	return evalplugin.Record{
		Plugin: &evalplugin.Plugin{
			APIVersion: evalplugin.PluginAPIVersion,
			Kind:       evalplugin.PluginKind,
			Metadata:   evalplugin.PluginMetadata{Name: name},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{
					Type:     evalplugin.ServiceLangSmith,
					Endpoint: "https://api.smith.langchain.com",
					Auth:     evalplugin.AuthSpec{SecretRef: name, KeyRef: "key"},
				},
				Send: evalplugin.SendSpec{
					Trigger:  "on_trace",
					Sampling: evalplugin.SamplingFraction(1.0),
				},
				Collect: evalplugin.CollectSpec{
					Mode: "webhook",
					Mapping: evalplugin.ResultMapping{
						Metric: "$.name",
						Score:  "$.score",
					},
				},
			},
		},
		Source:  evalplugin.Source{Kind: evalplugin.SourceHelm},
		Enabled: true,
	}
}
