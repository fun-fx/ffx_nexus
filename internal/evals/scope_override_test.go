package evals

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/ffxnexus/nexus/internal/observability"
)

// v0.6.9: Policy A scope override tests.
// `scope: user` profiles with enabled=false suppress `scope: org` profiles
// of the same kind for *that user's* traces. Traces belonging to other
// users still see the org-scoped profile. `scope: user` profiles with
// enabled=true remain additive (they run alongside the org profile).

func mkPIIProfile(id string, scope Scope, owner string, enabled bool) EvalProfile {
	return EvalProfile{
		ID:          id,
		Name:        id,
		Kind:        ProfileHeuristicPII,
		Scope:       scope,
		OwnerUserID: owner,
		SampleRate:  1.0,
		Enabled:     enabled,
		Endpoint:    EvalEndpoint{KeySource: KeySourceBuiltin},
	}
}

func newCollectTestWorker(t *testing.T) *Worker {
	t.Helper()
	return NewWorker(Options{
		Workers:         1,
		JudgeSampleRate: 0,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestScopeOverride_UserOffSuppressesOrgForUser: a user-scope profile of
// kind PII with enabled=false for owner=u-alice suppresses the org-scope
// PII profile for u-alice's trace.
func TestScopeOverride_UserOffSuppressesOrgForUser(t *testing.T) {
	w := newCollectTestWorker(t)
	w.ReplaceProfiles([]EvalProfile{
		mkPIIProfile("default-pii", ScopeOrg, "", true),
		mkPIIProfile("user-pii-alice", ScopeUser, "u-alice", false),
	})
	alice := observability.Trace{TraceID: "alice", UserID: "u-alice", StatusCode: 200}
	evs := w.collectEvaluators(alice, w.configuredProfiles, nil, 0, nil)
	if countKind(evs, "heuristic_pii") != 0 {
		t.Fatalf("alice's user-off profile must suppress org profile: %d pii evs", countKind(evs, "heuristic_pii"))
	}
}

// TestScopeOverride_DoesNotAffectOtherUsers: when alice creates a
// user-scope disable, bob's traffic is unaffected — bob still sees the
// org-scope PII evaluator.
func TestScopeOverride_DoesNotAffectOtherUsers(t *testing.T) {
	w := newCollectTestWorker(t)
	w.ReplaceProfiles([]EvalProfile{
		mkPIIProfile("default-pii", ScopeOrg, "", true),
		mkPIIProfile("user-pii-alice", ScopeUser, "u-alice", false),
	})
	bob := observability.Trace{TraceID: "bob", UserID: "u-bob", StatusCode: 200}
	evs := w.collectEvaluators(bob, w.configuredProfiles, nil, 0, nil)
	if countKind(evs, "heuristic_pii") != 1 {
		t.Fatalf("bob's org profile must run unaffected: %d pii evs", countKind(evs, "heuristic_pii"))
	}
}

// TestScopeOverride_UserON_AdditiveWithOrg: when a user-scope profile of
// the same kind is enabled=true it does NOT consume the org profile —
// both run, giving the user a second scoring path.
func TestScopeOverride_UserON_AdditiveWithOrg(t *testing.T) {
	w := newCollectTestWorker(t)
	w.ReplaceProfiles([]EvalProfile{
		mkPIIProfile("default-pii", ScopeOrg, "", true),
		mkPIIProfile("user-pii-alice", ScopeUser, "u-alice", true),
	})
	alice := observability.Trace{TraceID: "alice", UserID: "u-alice", StatusCode: 200}
	evs := w.collectEvaluators(alice, w.configuredProfiles, nil, 0, nil)
	if countKind(evs, "heuristic_pii") < 2 {
		t.Fatalf("both org and user profiles should run additively, got %d pii evs", countKind(evs, "heuristic_pii"))
	}
}

// TestScopeOverride_CompletenessAndPII: completeness follows the same
// override rule as PII. Alice turning off completeness does not affect
// PII nor bob's completeness evaluator.
func TestScopeOverride_CompletenessAndPII(t *testing.T) {
	w := newCollectTestWorker(t)
	complete := EvalProfile{
		ID: "default-completeness", Name: "default-completeness",
		Kind: ProfileHeuristicCompleteness, Scope: ScopeOrg,
		SampleRate: 1.0, Enabled: true,
		Endpoint: EvalEndpoint{KeySource: KeySourceBuiltin},
	}
	userComplete := EvalProfile{
		ID: "user-complete-alice", Name: "user-complete-alice",
		Kind: ProfileHeuristicCompleteness, Scope: ScopeUser, OwnerUserID: "u-alice",
		SampleRate: 1.0, Enabled: false,
		Endpoint: EvalEndpoint{KeySource: KeySourceBuiltin},
	}
	w.ReplaceProfiles([]EvalProfile{
		mkPIIProfile("default-pii", ScopeOrg, "", true),
		complete,
		userComplete,
	})
	alice := observability.Trace{TraceID: "alice", UserID: "u-alice", StatusCode: 200}
	evs := w.collectEvaluators(alice, w.configuredProfiles, nil, 0, nil)
	if countKind(evs, "heuristic_completeness") != 0 {
		t.Fatalf("user-off completeness must suppress org completeness: %d", countKind(evs, "heuristic_completeness"))
	}
	if countKind(evs, "heuristic_pii") != 1 {
		t.Fatalf("pii must still run for alice (different kind): %d", countKind(evs, "heuristic_pii"))
	}
	bob := observability.Trace{TraceID: "bob", UserID: "u-bob", StatusCode: 200}
	bobEvs := w.collectEvaluators(bob, w.configuredProfiles, nil, 0, nil)
	if countKind(bobEvs, "heuristic_pii") != 1 || countKind(bobEvs, "heuristic_completeness") != 1 {
		t.Fatalf("bob unaffected: pii=%d complete=%d", countKind(bobEvs, "heuristic_pii"), countKind(bobEvs, "heuristic_completeness"))
	}
}

// countKind groups evaluators using the Evaluator's Name() string so we
// don't pollute the API surface with a kind-mapping helper.
func countKind(evs []Evaluator, name string) int {
	n := 0
	for _, e := range evs {
		if e.Name() == name {
			n++
		}
	}
	return n
}

// silence import warning while keeping the package's Trace context in
// reach for future tests.
var _ = context.Background
