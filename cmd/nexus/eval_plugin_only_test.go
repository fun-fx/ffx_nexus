package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evals"
)

func strptr(s string) *string { return &s }

// pluginOnlyConfig is the shape production runs today: the flag is on
// while the values file still points at the in-cluster judge and the
// Python sidecar, because those env vars outlived the decision.
func pluginOnlyConfig() config.Config {
	return config.Config{
		EvalPluginOnly:     true,
		JudgeBaseURL:       "http://ollama:11434/v1",
		JudgeModel:         "qwen2.5:7b-instruct",
		EvalServiceURL:     "http://eval-service:8200",
		EvalServiceMetrics: "answer_relevancy,toxicity,bias",
		EvalSampleRate:     1.0,
		EvalWorkers:        1,
	}
}

// TestBuildEvalWorkerPluginOnlyDropsInClusterCompute is the core of this
// change: the flag used to skip profile seeding only, so a deployment
// could report plugin-only and still call ollama and eval-service on
// every sampled trace.
func TestBuildEvalWorkerPluginOnlyDropsInClusterCompute(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	w := buildEvalWorker(pluginOnlyConfig(), nil, nil, evals.StoreNoop, "none", log)
	defer w.Close(context.Background())

	state := w.RuntimeState()
	if state.JudgeBaseURL != "" {
		t.Errorf("judge base URL must not be wired under plugin-only, got %q", state.JudgeBaseURL)
	}
	if state.RemoteURL != "" {
		t.Errorf("eval-service URL must not be wired under plugin-only, got %q", state.RemoteURL)
	}
	if !w.PluginOnly() {
		t.Error("worker should report plugin-only")
	}

	out := buf.String()
	// Silence here reads exactly like a healthy judge in the boot log,
	// so the ignored settings have to be named.
	if !strings.Contains(out, "ignoring in-cluster eval compute settings") {
		t.Fatalf("the ignored settings must be reported: %s", out)
	}
	for _, want := range []string{"ollama", "eval-service"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning should name %s: %s", want, out)
		}
	}
	if strings.Contains(out, "external eval service enabled") {
		t.Errorf("plugin-only must not report the sidecar as enabled: %s", out)
	}
}

// TestBuildEvalWorkerWithoutFlagKeepsInClusterCompute guards the
// on-prem deployment that deliberately runs its own judge.
func TestBuildEvalWorkerWithoutFlagKeepsInClusterCompute(t *testing.T) {
	cfg := pluginOnlyConfig()
	cfg.EvalPluginOnly = false
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	w := buildEvalWorker(cfg, nil, nil, evals.StoreNoop, "none", log)
	defer w.Close(context.Background())

	state := w.RuntimeState()
	if state.JudgeBaseURL != cfg.JudgeBaseURL {
		t.Errorf("judge base URL = %q, want %q", state.JudgeBaseURL, cfg.JudgeBaseURL)
	}
	if state.RemoteURL != cfg.EvalServiceURL {
		t.Errorf("eval-service URL = %q, want %q", state.RemoteURL, cfg.EvalServiceURL)
	}
	if w.PluginOnly() {
		t.Error("worker should not report plugin-only when the flag is off")
	}
}

// TestApplyRejectsEvalComputePatchUnderPluginOnly: accepting the patch
// would return a snapshot that looks applied while nothing was wired.
func TestApplyRejectsEvalComputePatchUnderPluginOnly(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := buildEvalWorker(pluginOnlyConfig(), nil, nil, evals.StoreNoop, "none", log)
	defer w.Close(context.Background())
	c := &evalRuntimeController{cfg: pluginOnlyConfig(), worker: w}

	patches := map[string]console.EvalConfigPatch{
		"judge base url":  {JudgeBaseURL: strptr("http://ollama:11434/v1")},
		"judge model":     {JudgeModel: strptr("qwen2.5:7b-instruct")},
		"judge api key":   {JudgeAPIKey: strptr("secret")},
		"sidecar url":     {EvalServiceURL: strptr("http://eval-service:8200")},
		"sidecar metrics": {EvalServiceMetrics: strptr("toxicity")},
	}
	for name, patch := range patches {
		if _, err := c.Apply(patch); err == nil {
			t.Errorf("%s: patch should be rejected under plugin-only", name)
		} else if !strings.Contains(err.Error(), "plugin-only") {
			t.Errorf("%s: error should explain the mode, got %v", name, err)
		}
	}

	// Unrelated cells stay editable — the mode is about eval compute,
	// not about freezing the whole config surface.
	rate := 0.5
	if _, err := c.Apply(console.EvalConfigPatch{SampleRate: &rate}); err != nil {
		t.Errorf("sample rate should remain patchable: %v", err)
	}
}

// TestConfigureJudgesRefusedUnderPluginOnly covers the direct caller
// path that bypasses the API guard.
func TestConfigureJudgesRefusedUnderPluginOnly(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := buildEvalWorker(pluginOnlyConfig(), nil, nil, evals.StoreNoop, "none", log)
	defer w.Close(context.Background())

	w.ConfigureJudges(
		evals.JudgeRuntimeConfig{BaseURL: "http://ollama:11434/v1", Model: "qwen2.5:7b-instruct"},
		evals.RemoteRuntimeConfig{URL: "http://eval-service:8200"},
	)
	state := w.RuntimeState()
	if state.JudgeBaseURL != "" || state.RemoteURL != "" {
		t.Fatalf("reconfiguration must not wire eval compute: judge=%q remote=%q",
			state.JudgeBaseURL, state.RemoteURL)
	}
}

// TestSaveEvalProfileRejectsComputeKindsUnderPluginOnly: an enabled row
// that never scores is the exact failure mode this change removes, so
// the write is refused rather than stored.
func TestSaveEvalProfileRejectsComputeKindsUnderPluginOnly(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := buildEvalWorker(pluginOnlyConfig(), nil, nil, evals.StoreNoop, "none", log)
	defer w.Close(context.Background())
	store := newFakeProfileStore()
	c := &evalRuntimeController{cfg: pluginOnlyConfig(), profileStore: store, worker: w}

	for _, kind := range []evals.ProfileKind{evals.ProfileSLMJudge, evals.ProfileRemoteEval} {
		err := c.SaveEvalProfile(context.Background(), &evals.EvalProfile{
			ID: "p-" + string(kind), Name: string(kind), Kind: kind, Enabled: true,
		})
		if err == nil {
			t.Errorf("kind %q should be refused under plugin-only", kind)
		}
		if _, ok := store.rows["p-"+string(kind)]; ok {
			t.Errorf("kind %q must not be stored", kind)
		}
	}

	// The zero-egress heuristic kinds are unaffected.
	if err := c.SaveEvalProfile(context.Background(), &evals.EvalProfile{
		ID: "p-pii", Name: "pii", Kind: evals.ProfileHeuristicPII, Enabled: true,
	}); err != nil {
		t.Fatalf("heuristic profile should still save: %v", err)
	}
}
