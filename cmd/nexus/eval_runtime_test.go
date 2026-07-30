package main

import (
	"testing"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/evals"
)

// TestEnvVarSeedProfiles_DefaultSeedsHeuristic verifies that an
// empty store + default config produces both heuristic profiles
// plus the legacy defaults (when env vars describe them).
func TestEnvVarSeedProfiles_DefaultSeedsHeuristic(t *testing.T) {
	cfg := config.Config{
		JudgeBaseURL:       "http://judge.local",
		JudgeModel:         "judge-v1",
		EvalSampleRate:     1.0,
		EvalServiceURL:     "http://remote.local",
		EvalServiceMetrics: "faithfulness",
		EvalPluginOnly:     false,
	}
	profiles := envVarSeedProfiles(cfg)
	kinds := map[evals.ProfileKind]bool{}
	for _, p := range profiles {
		kinds[p.Kind] = true
	}
	want := []evals.ProfileKind{
		evals.ProfileSLMJudge,
		evals.ProfileRemoteEval,
		evals.ProfileHeuristicPII,
		evals.ProfileHeuristicCompleteness,
	}
	for _, w := range want {
		if !kinds[w] {
			t.Errorf("default seed missing kind=%q; got=%v", w, kinds)
		}
	}
}

// TestEnvVarSeedProfiles_PluginOnlySkipsHeuristic verifies that
// NEXUS_EVAL_PLUGIN_ONLY=true suppresses every profile whose kind
// is "in-cluster" — heuristic + slm_judge + remote_eval — while
// leaving the seed path open for plugin-managed profiles that the
// admin will create later through the console.
func TestEnvVarSeedProfiles_PluginOnlySkipsHeuristic(t *testing.T) {
	cfg := config.Config{
		JudgeBaseURL:       "http://judge.local",
		JudgeModel:         "judge-v1",
		EvalSampleRate:     1.0,
		EvalServiceURL:     "http://remote.local",
		EvalServiceMetrics: "faithfulness",
		EvalPluginOnly:     true,
	}
	profiles := envVarSeedProfiles(cfg)
	if len(profiles) != 0 {
		var kinds []string
		for _, p := range profiles {
			kinds = append(kinds, string(p.Kind))
		}
		t.Errorf("plugin-only mode must yield zero seeded profiles; got=%v", kinds)
	}
}

// TestEnvVarSeedProfiles_EmptyConfigNoHeuristic verifies that
// missing env vars (an explicit "no in-cluster eval") does not
// silently inject heuristic rows — they only seed when both
// the legacy judge AND the heuristic auto-seed path are silent.
func TestEnvVarSeedProfiles_EmptyConfigOnlyHeuristic(t *testing.T) {
	cfg := config.Config{} // nothing set
	profiles := envVarSeedProfiles(cfg)
	kinds := map[evals.ProfileKind]bool{}
	for _, p := range profiles {
		kinds[p.Kind] = true
	}
	want := []evals.ProfileKind{evals.ProfileHeuristicPII, evals.ProfileHeuristicCompleteness}
	for _, w := range want {
		if !kinds[w] {
			t.Errorf("empty config still seeds heuristic; missing kind=%q; got=%v", w, kinds)
		}
	}
	if kinds[evals.ProfileSLMJudge] || kinds[evals.ProfileRemoteEval] {
		t.Errorf("empty config unexpectedly seeded a legacy profile; got=%v", kinds)
	}
}
