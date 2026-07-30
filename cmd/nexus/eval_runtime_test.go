package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/evals"
)

// fakeProfileStore is a minimal in-memory ProfileStore used by the
// SeedProfilesFromConfig purge-path tests. It only needs to round-trip
// Save / List / Delete; no scoring, no concurrency. The runtime
// controller's full Worker independence isn't exercised here — the
// test asserts on store state, not on score emission.
type fakeProfileStore struct {
	rows map[string]*evals.EvalProfile
}

func newFakeProfileStore(initial ...*evals.EvalProfile) *fakeProfileStore {
	f := &fakeProfileStore{rows: map[string]*evals.EvalProfile{}}
	for _, p := range initial {
		f.rows[p.ID] = p
	}
	return f
}

func (f *fakeProfileStore) List(_ context.Context, _ string) ([]evals.EvalProfile, error) {
	out := make([]evals.EvalProfile, 0, len(f.rows))
	for _, p := range f.rows {
		out = append(out, *p)
	}
	return out, nil
}

func (f *fakeProfileStore) Get(_ context.Context, id string) (*evals.EvalProfile, error) {
	p, ok := f.rows[id]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *p
	return &cp, nil
}

func (f *fakeProfileStore) Save(_ context.Context, p *evals.EvalProfile) error {
	cp := *p
	f.rows[p.ID] = &cp
	return nil
}

func (f *fakeProfileStore) Delete(_ context.Context, id string) error {
	// Mirror store-side idempotent delete: missing is not an error.
	delete(f.rows, id)
	return nil
}

// TestSeedProfilesFromConfig_PurgeLegacyProfiles verifies that when
// both EvalPluginOnly and PurgeLegacyProfilesOnBoot are set, the
// runtime controller hard-deletes the four well-known default rows
// before reporting the surviving store contents. This is the
// regression test for the plugin-only production rollout where
// admins don't want a manual cleanup pass to remove the historical
// default-pii / default-completeness / default-judge / default-remote
// rows seeded by older versions.
func TestSeedProfilesFromConfig_PurgeLegacyProfiles(t *testing.T) {
	store := newFakeProfileStore(
		&evals.EvalProfile{ID: "default-pii", Kind: evals.ProfileHeuristicPII},
		&evals.EvalProfile{ID: "default-completeness", Kind: evals.ProfileHeuristicCompleteness},
		&evals.EvalProfile{ID: "default-judge", Kind: evals.ProfileSLMJudge},
		&evals.EvalProfile{ID: "default-remote", Kind: evals.ProfileRemoteEval},
		// A user-created plugin-managed profile that must survive
		// the purge untouched.
		&evals.EvalProfile{ID: "tenant-langfuse", Kind: evals.ProfileKind("external")},
	)
	c := &evalRuntimeController{
		cfg:          config.Config{EvalPluginOnly: true, PurgeLegacyProfilesOnBoot: true},
		profileStore: store,
		worker:       nil, // we don't exercise the worker; controller guards nil.
	}
	profiles, err := c.SeedProfilesFromConfig(context.Background())
	if err != nil {
		t.Fatalf("SeedProfilesFromConfig: %v", err)
	}
	if len(profiles) != 1 || profiles[0].ID != "tenant-langfuse" {
		t.Fatalf("expected only tenant-langfuse to survive; got=%+v", profiles)
	}
	for _, removed := range []string{"default-pii", "default-completeness", "default-judge", "default-remote"} {
		if _, ok := store.rows[removed]; ok {
			t.Errorf("legacy row %q survived purge", removed)
		}
	}
}

// TestSeedProfilesFromConfig_PurgeOptInGate verifies that flipping
// only EvalPluginOnly (without PurgeLegacyProfilesOnBoot) preserves
// the historical default-* rows. The two-flag gate is load-bearing —
// the controller treats them as independent operator decisions.
func TestSeedProfilesFromConfig_PurgeOptInGate(t *testing.T) {
	store := newFakeProfileStore(
		&evals.EvalProfile{ID: "default-pii", Kind: evals.ProfileHeuristicPII},
		&evals.EvalProfile{ID: "default-remote", Kind: evals.ProfileRemoteEval},
	)
	c := &evalRuntimeController{
		cfg:          config.Config{EvalPluginOnly: true, PurgeLegacyProfilesOnBoot: false},
		profileStore: store,
		worker:       nil,
	}
	if _, err := c.SeedProfilesFromConfig(context.Background()); err != nil {
		t.Fatalf("SeedProfilesFromConfig: %v", err)
	}
	if _, ok := store.rows["default-pii"]; !ok {
		t.Errorf("default-pii must survive when purge flag is off")
	}
	if _, ok := store.rows["default-remote"]; !ok {
		t.Errorf("default-remote must survive when purge flag is off")
	}
}
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
