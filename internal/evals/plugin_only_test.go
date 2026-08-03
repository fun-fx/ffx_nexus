package evals

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

func pluginOnlyWorker(t *testing.T, pluginOnly bool) *Worker {
	t.Helper()
	return NewWorker(Options{
		Workers:         1,
		JudgeSampleRate: 1,
		PluginOnly:      pluginOnly,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func judgeProfile(t *testing.T, kind ProfileKind) EvalProfile {
	t.Helper()
	return EvalProfile{
		ID: "p-" + string(kind), Name: string(kind), Kind: kind,
		Scope: ScopeOrg, SampleRate: 1.0, Enabled: true,
		Endpoint: EvalEndpoint{
			BaseURL: "http://ollama:11434/v1", Model: "qwen2.5:7b-instruct",
			KeySource: KeySourceBuiltin,
		},
	}
}

// alwaysResolve stands in for the runtime controller's secret hook so
// the judge branch is reachable in the non-plugin-only case.
func alwaysResolve(_ observability.Trace, _ EvalEndpoint) (string, error) {
	return "secret", nil
}

// TestCollectEvaluatorsSkipsComputeProfilesUnderPluginOnly: a profile
// that survived in the store, or one an admin created directly against
// the API, must not put an in-cluster judge back on the trace path.
func TestCollectEvaluatorsSkipsComputeProfilesUnderPluginOnly(t *testing.T) {
	w := pluginOnlyWorker(t, true)
	defer w.Close(context.Background())

	profiles := []EvalProfile{
		judgeProfile(t, ProfileSLMJudge),
		judgeProfile(t, ProfileRemoteEval),
	}
	got := w.collectEvaluators(observability.Trace{TraceID: "t1"}, profiles, nil, 1, alwaysResolve)
	if len(got) != 0 {
		var names []string
		for _, e := range got {
			names = append(names, e.Name())
		}
		t.Fatalf("plugin-only must yield no compute evaluators; got %v", names)
	}
}

// TestCollectEvaluatorsKeepsComputeProfilesWithoutFlag is the control:
// the same profiles still work for a deployment that runs its own judge.
func TestCollectEvaluatorsKeepsComputeProfilesWithoutFlag(t *testing.T) {
	w := pluginOnlyWorker(t, false)
	defer w.Close(context.Background())

	profiles := []EvalProfile{
		judgeProfile(t, ProfileSLMJudge),
		judgeProfile(t, ProfileRemoteEval),
	}
	got := w.collectEvaluators(observability.Trace{TraceID: "t1"}, profiles, nil, 1, alwaysResolve)
	if len(got) != 2 {
		t.Fatalf("expected judge + remote evaluators, got %d", len(got))
	}
}

// TestCollectEvaluatorsKeepsHeuristicsUnderPluginOnly: the Go
// heuristics cost no egress and no hosted compute, so plugin-only has
// no reason to drop them. PII in particular runs before egress.
func TestCollectEvaluatorsKeepsHeuristicsUnderPluginOnly(t *testing.T) {
	w := pluginOnlyWorker(t, true)
	defer w.Close(context.Background())

	profiles := []EvalProfile{
		{ID: "default-pii", Kind: ProfileHeuristicPII, Scope: ScopeOrg, SampleRate: 1, Enabled: true},
		{ID: "default-completeness", Kind: ProfileHeuristicCompleteness, Scope: ScopeOrg, SampleRate: 1, Enabled: true},
	}
	got := w.collectEvaluators(observability.Trace{TraceID: "t1"}, profiles, nil, 1, alwaysResolve)
	if len(got) != 2 {
		t.Fatalf("heuristics must survive plugin-only; got %d", len(got))
	}
}

// TestNewWorkerDropsPreBuiltJudgesUnderPluginOnly: PluginOnly plus a
// judge slice are contradictory inputs, and honouring the slice would
// leave the "no hosted eval compute" invariant false for the struct.
func TestNewWorkerDropsPreBuiltJudgesUnderPluginOnly(t *testing.T) {
	judge := NewSLMJudge(JudgeConfig{BaseURL: "http://ollama:11434/v1", Model: "m"})
	if judge == nil {
		t.Fatal("precondition: judge should build from a base URL")
	}
	w := NewWorker(Options{
		Workers: 1, JudgeSampleRate: 1, PluginOnly: true,
		Judges: []Evaluator{judge},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer w.Close(context.Background())

	got := w.collectEvaluators(observability.Trace{TraceID: "t1"}, nil, w.judges, 1, alwaysResolve)
	if len(got) != 0 {
		t.Fatalf("pre-built judges must be dropped under plugin-only; got %d", len(got))
	}
}

// TestConfigureJudgesNoopUnderPluginOnly guards the runtime door: the
// console PATCH path is rejected upstream, this is the direct caller.
func TestConfigureJudgesNoopUnderPluginOnly(t *testing.T) {
	w := pluginOnlyWorker(t, true)
	defer w.Close(context.Background())

	w.ConfigureJudges(
		JudgeRuntimeConfig{BaseURL: "http://ollama:11434/v1", Model: "m"},
		RemoteRuntimeConfig{URL: "http://eval-service:8200", Timeout: time.Second},
	)
	state := w.RuntimeState()
	if state.JudgeBaseURL != "" || state.RemoteURL != "" {
		t.Fatalf("plugin-only must refuse reconfiguration: judge=%q remote=%q",
			state.JudgeBaseURL, state.RemoteURL)
	}
	if len(w.judges) != 0 {
		t.Fatalf("no judges should be wired; got %d", len(w.judges))
	}
}
