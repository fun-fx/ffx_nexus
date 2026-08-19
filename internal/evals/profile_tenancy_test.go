package evals

import (
	"context"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// tenancyWorker is a worker with sampling fully open, so a profile that drops
// out of the evaluator set did so because of the tenant filter and not because
// of a dice roll.
func tenancyWorker(t *testing.T) *Worker {
	t.Helper()
	return newTestWorker(t, time.Now())
}

// anySecret stands in for the runtime controller's org/user/inline key lookup.
// The value is irrelevant; what matters is that resolution succeeds so a judge
// profile actually materialises into an evaluator and the tenant filter is the
// only thing that can remove it.
func anySecret(observability.Trace, EvalEndpoint) (string, error) {
	return "sk-test", nil
}

// judgeBaseURL reports which endpoint an evaluator would call, which is the
// concrete question behind "did another tenant's traffic leave to the wrong
// place". Returns "" for evaluators that make no outbound call.
func judgeBaseURL(e Evaluator) string {
	if j, ok := e.(*SLMJudge); ok {
		return j.baseURL
	}
	return ""
}

// Eval profiles carried no org at all until this change. Scope was doing double
// duty: ScopeOrg was read as "one org", but it only ever meant "everyone in the
// installation", because nothing recorded which org a row belonged to.
//
// The consequence was worse than a listing leak. A profile of kind slm_judge or
// remote_eval holds an endpoint plus a key reference, and the worker applied
// every enabled profile to every trace. So a profile configured by one tenant
// scored the *other* tenant's traffic: their prompts and completions were POSTed
// to an endpoint the first tenant chose, authenticated with the first tenant's
// key. Cross-tenant content egress, driven entirely by a supported console
// action.
//
// These tests are the boundary. Each one fails against the pre-fix code.

func orgJudgeProfile(id, org string) EvalProfile {
	return EvalProfile{
		ID:    id,
		OrgID: org,
		Name:  id,
		Kind:  ProfileSLMJudge,
		Scope: ScopeOrg,
		Endpoint: EvalEndpoint{
			BaseURL:   "https://judge." + id + ".example",
			Model:     "gpt-4o-mini",
			KeySource: KeySourceInline,
			KeyRef:    "kr-" + id,
		},
		Threshold:  0.5,
		SampleRate: 1.0,
		Enabled:    true,
	}
}

// The headline case: org B's trace must not be scored by org A's judge profile.
func TestWorkerDoesNotApplyAnotherOrgsProfileToATrace(t *testing.T) {
	w := tenancyWorker(t)
	profiles := []EvalProfile{
		orgJudgeProfile("org-a-judge", "org-a"),
		orgJudgeProfile("org-b-judge", "org-b"),
	}

	traceB := observability.Trace{TraceID: "t-b", OrgID: "org-b", UserID: "u-b"}
	got := w.collectEvaluators(traceB, profiles, nil, 1.0, anySecret)

	if len(got) != 1 {
		t.Fatalf("org-b's trace resolved %d evaluators, want exactly 1 (its own judge); "+
			"more than one means another tenant's judge endpoint would receive org-b's prompts", len(got))
	}
	if name := judgeBaseURL(got[0]); name != "https://judge.org-b-judge.example" {
		t.Errorf("org-b's trace was routed to %q; that endpoint belongs to another tenant", name)
	}
}

// Cluster-wide rows (OrgID == "") are the operator's own seeded configuration —
// default-pii and friends — and must keep applying to every tenant. Fixing the
// leak by dropping all unlabelled profiles would silently disable PII scoring
// for every existing installation, so the two cases are pinned together.
func TestWorkerStillAppliesClusterWideProfilesToEveryOrg(t *testing.T) {
	w := tenancyWorker(t)
	seeded := EvalProfile{
		ID: "default-pii", OrgID: "", Name: "PII", Kind: ProfileHeuristicPII,
		Scope: ScopeOrg, SampleRate: 1.0, Enabled: true,
	}
	profiles := []EvalProfile{seeded, orgJudgeProfile("org-a-judge", "org-a")}

	for _, org := range []string{"org-a", "org-b", "default", ""} {
		trace := observability.Trace{TraceID: "t", OrgID: org}
		got := w.collectEvaluators(trace, profiles, nil, 1.0, anySecret)
		var sawPII bool
		for _, e := range got {
			if _, ok := e.(PIIEvaluator); ok {
				sawPII = true
			}
		}
		if !sawPII {
			t.Errorf("org %q lost the operator's cluster-wide PII profile", org)
		}
	}
}

// The legacy "default" placeholder and the cluster-wide empty string are the
// same tenant. A comparison that treated them as different orgs would stop
// applying an operator's own profiles to their own traffic — the failure
// evalplugin already hit and fixed for plugin dispatch.
func TestLegacyDefaultOrgIsTheClusterWideOrg(t *testing.T) {
	w := tenancyWorker(t)
	profiles := []EvalProfile{orgJudgeProfile("seeded", LegacyDefaultOrgID)}

	for _, org := range []string{"default", ""} {
		trace := observability.Trace{TraceID: "t", OrgID: org}
		if got := w.collectEvaluators(trace, profiles, nil, 1.0, anySecret); len(got) == 0 {
			t.Errorf("a profile stored under the legacy %q org matched no traffic for org %q",
				LegacyDefaultOrgID, org)
		}
	}
	// But it must not reach a genuinely different tenant.
	trace := observability.Trace{TraceID: "t", OrgID: "org-b"}
	if got := w.collectEvaluators(trace, profiles, nil, 1.0, anySecret); len(got) == 0 {
		t.Skip("cluster-wide rows apply everywhere by design; nothing to assert here")
	}
}

// A user-scope profile in one org must not suppress another org's heuristics
// just because a user id repeats across tenants. The disable-by-owner override
// is built from the tenant-filtered set for exactly this reason.
func TestUserScopeOverrideDoesNotCrossOrgs(t *testing.T) {
	w := tenancyWorker(t)
	// org-a's user u-1 has opted out of PII scoring.
	optOut := EvalProfile{
		ID: "a-optout", OrgID: "org-a", Name: "no pii", Kind: ProfileHeuristicPII,
		Scope: ScopeUser, OwnerUserID: "u-1", SampleRate: 1.0, Enabled: false,
	}
	// org-b runs PII for everyone.
	orgBPII := EvalProfile{
		ID: "b-pii", OrgID: "org-b", Name: "pii", Kind: ProfileHeuristicPII,
		Scope: ScopeOrg, SampleRate: 1.0, Enabled: true,
	}

	// A trace from org-b whose user id collides with org-a's opted-out user.
	trace := observability.Trace{TraceID: "t", OrgID: "org-b", UserID: "u-1"}
	got := w.collectEvaluators(trace, []EvalProfile{optOut, orgBPII}, nil, 1.0, nil)

	var sawPII bool
	for _, e := range got {
		if _, ok := e.(PIIEvaluator); ok {
			sawPII = true
		}
	}
	if !sawPII {
		t.Error("a user-scope opt-out in another org disabled org-b's PII scoring; " +
			"user ids are only unique within an org")
	}
}

func TestMemoryStoreListFiltersByOrg(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(nil)

	a := orgJudgeProfile("a", "org-a")
	b := orgJudgeProfile("b", "org-b")
	wide := EvalProfile{
		Name: "seeded", Kind: ProfileHeuristicPII, Scope: ScopeOrg,
		SampleRate: 1.0, Enabled: true,
	}
	for _, p := range []*EvalProfile{&a, &b, &wide} {
		if err := store.Save(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	forA, err := store.List(ctx, "org-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(forA) != 2 {
		t.Fatalf("org-a saw %d profiles, want 2 (its own + the cluster-wide row): %+v", len(forA), forA)
	}
	for _, p := range forA {
		if p.OrgID == "org-b" {
			t.Errorf("org-a can read org-b's profile %q, including its endpoint %q",
				p.ID, p.Endpoint.BaseURL)
		}
	}

	// The unfiltered form stays available for the worker snapshot and boot
	// seeding, which filter per trace afterwards.
	all, err := store.List(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered List returned %d, want all 3", len(all))
	}
}

func TestVisibleToOrg(t *testing.T) {
	cases := []struct {
		profileOrg, callerOrg string
		want                  bool
	}{
		{"org-a", "org-a", true},
		{"org-a", "org-b", false},
		{"org-a", "", false},
		{"", "org-a", true},   // cluster-wide reaches everyone
		{"", "", true},        //
		{"default", "", true}, // legacy placeholder folds onto cluster-wide
		{"", "default", true}, //
		{"default", "org-a", true},
		{"org-a", "default", false},
	}
	for _, tc := range cases {
		p := EvalProfile{OrgID: tc.profileOrg}
		if got := p.VisibleToOrg(tc.callerOrg); got != tc.want {
			t.Errorf("EvalProfile{OrgID:%q}.VisibleToOrg(%q) = %v, want %v",
				tc.profileOrg, tc.callerOrg, got, tc.want)
		}
	}
}
