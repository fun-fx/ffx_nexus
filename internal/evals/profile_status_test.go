package evals

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// When an admin disables a profile of kind ProfileSLMJudge / RemoteEval
// via the console, the worker's ProfileStatus must report its absence.
// This is the round-trip guarantee that the new "Disable evaluation"
// UI relies on for an immediate, restart-free effect.
func TestProfileStatus_RespectsProfileEnabled(t *testing.T) {
	t0 := time.Date(2026, 7, 27, 15, 0, 0, 0, time.UTC)
	w := newTestWorker(t, t0)

	// Profile-enabled=true path: judge + remote both present.
	w.ReplaceProfiles([]EvalProfile{
		mustSaveProfile(t, t0, ProfileSLMJudge, true),
		mustSaveProfile(t, t0, ProfileRemoteEval, true),
	})
	ps := w.ProfileStatus()
	if !ps.SLMJudgeEnabled {
		t.Fatalf("with profile enabled, expect SLMJudgeEnabled=true; got %+v", ps)
	}
	if !ps.RemoteEvalEnabled {
		t.Fatalf("with profile enabled, expect RemoteEvalEnabled=true; got %+v", ps)
	}

	// Disable both via ReplaceProfiles — same array shape the runtime
	// controller uses after a `disable evaluation` click.
	w.ReplaceProfiles([]EvalProfile{
		mustSaveProfile(t, t0, ProfileSLMJudge, false),
		mustSaveProfile(t, t0, ProfileRemoteEval, false),
	})
	ps = w.ProfileStatus()
	if ps.SLMJudgeEnabled {
		t.Fatalf("with profile enabled=false, want SLMJudgeEnabled=false; got %+v", ps)
	}
	if ps.RemoteEvalEnabled {
		t.Fatalf("with profile enabled=false, want RemoteEvalEnabled=false; got %+v", ps)
	}
}

// Same round-trip for RemoteEval — making sure the two kinds are
// independent: disabling SLM judge must not flip remote eval status, and
// vice versa.
func TestProfileStatus_TwoKindsIndependent(t *testing.T) {
	t0 := time.Date(2026, 7, 27, 15, 1, 0, 0, time.UTC)
	w := newTestWorker(t, t0)

	w.ReplaceProfiles([]EvalProfile{
		mustSaveProfile(t, t0, ProfileSLMJudge, true),
		mustSaveProfile(t, t0, ProfileRemoteEval, false),
	})
	ps := w.ProfileStatus()
	if !ps.SLMJudgeEnabled {
		t.Fatalf("SLMJudge profile enabled=true; want ProfileStatus().SLMJudgeEnabled=true, got %+v", ps)
	}
	if ps.RemoteEvalEnabled {
		t.Fatalf("RemoteEval profile enabled=false; want ProfileStatus().RemoteEvalEnabled=false, got %+v", ps)
	}

	// Flip remote on, judge off — independent.
	w.ReplaceProfiles([]EvalProfile{
		mustSaveProfile(t, t0, ProfileSLMJudge, false),
		mustSaveProfile(t, t0, ProfileRemoteEval, true),
	})
	ps = w.ProfileStatus()
	if ps.SLMJudgeEnabled {
		t.Fatalf("want SLMJudgeEnabled=false, got %+v", ps)
	}
	if !ps.RemoteEvalEnabled {
		t.Fatalf("want RemoteEvalEnabled=true, got %+v", ps)
	}
}

// Profile-driven collection must not be silently shadowed by the legacy
// "global" judges slice. After ReplaceProfiles, the worker reads only
// the profile set; legacy judges (if any were left around) are appended
// for back-compat and must not register a second copy that would re-
// enable the evaluator in spite of an admin-disable.
func TestProfileStatus_IgnoresLegacyJudgesField(t *testing.T) {
	t0 := time.Date(2026, 7, 27, 15, 2, 0, 0, time.UTC)
	w := newTestWorker(t, t0)
	// Seed legacy judges too (ConfigureJudges behavior) so we exercise
	// the co-exist path.
	w.ConfigureJudges(
		JudgeRuntimeConfig{BaseURL: "http://legacy-judge", Model: "x"},
		RemoteRuntimeConfig{URL: "http://legacy-remote"},
	)
	// No profile snapshot — legacy path only.
	ps := w.ProfileStatus()
	if ps.SLMJudgeEnabled || ps.RemoteEvalEnabled {
		t.Fatalf("without profiles, ProfileStatus should both be false; got %+v", ps)
	}

	// Add a disabled profile; legacy still wired but disabled wins.
	w.ReplaceProfiles([]EvalProfile{
		mustSaveProfile(t, t0, ProfileSLMJudge, false),
	})
	ps = w.ProfileStatus()
	if ps.SLMJudgeEnabled {
		t.Fatalf("disabled profile wins over legacy judges; got %+v", ps)
	}
}

// helpers

func newTestWorker(t *testing.T, _ time.Time) *Worker {
	t.Helper()
	return NewWorker(Options{
		Workers:         1,
		JudgeSampleRate: 0,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func mustSaveProfile(t *testing.T, t0 time.Time, kind ProfileKind, enabled bool) EvalProfile {
	t.Helper()
	store := NewMemoryStore(func() time.Time { return t0 })
	p := &EvalProfile{
		Name: string(kind), Kind: kind, Scope: ScopeOrg,
		SampleRate: 1.0, Enabled: enabled,
		Endpoint: EvalEndpoint{
			BaseURL:   "http://localhost:8200",
			Model:     "x",
			KeySource: KeySourceBuiltin,
		},
	}
	if err := store.Save(context.Background(), p); err != nil {
		t.Fatalf("Save should accept disabled profile: %v", err)
	}
	return *p
}
