package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evals"
)

// Object-level authorization: given a valid session in org A, can the caller act
// on an object id belonging to org B?
//
// Route-level guards do not answer this. Every route exercised here is
// admin-only and the caller here IS an admin — of the wrong tenant. The
// authorization that matters is per object, and the only way to know it is
// present is to hand a handler someone else's id and watch what it does.
//
// The refusal is 404 rather than 403 throughout. A 403 that says "that belongs
// to another org" confirms the id exists, which turns a guess into an oracle:
// walk the id space, keep the 403s, and you have enumerated another tenant's
// objects without ever reading one.

const (
	callerOrg  = "org-caller"
	victimOrg  = "org-victim"
	victimName = "victim-secret-name"
)

// orgSessionMux serves one route with a session in callerOrg.
func orgSessionMux(pattern, method string, h func(http.ResponseWriter, *http.Request, core.User)) http.Handler {
	r := chi.NewRouter()
	handler := func(w http.ResponseWriter, req *http.Request) {
		user := core.User{ID: "u-caller", Role: core.RoleAdmin, Email: "admin@caller.example", OrgID: callerOrg}
		ctx := context.WithValue(req.Context(), userCtxKey{}, user)
		h(w, req.WithContext(ctx), user)
	}
	switch method {
	case http.MethodGet:
		r.Get(pattern, handler)
	case http.MethodPatch:
		r.Patch(pattern, handler)
	case http.MethodDelete:
		r.Delete(pattern, handler)
	case http.MethodPost:
		r.Post(pattern, handler)
	}
	return r
}

// --- eval profiles ---------------------------------------------------------
//
// Profiles are the highest-consequence object here. A profile holds an endpoint
// and a key reference, and the worker applies it to traces. Writing into another
// org's profile set does not just corrupt their config: it redirects their
// prompts and completions to an endpoint of the attacker's choosing.

func profileServer(t *testing.T, seed ...*evals.EvalProfile) *Server {
	t.Helper()
	srv := newTestServer()
	srv.SetEvalProfiles(newStubProfileStore(seed...))
	return srv
}

func victimProfile() *evals.EvalProfile {
	return &evals.EvalProfile{
		ID:    "ep-victim",
		OrgID: victimOrg,
		Name:  victimName,
		Kind:  evals.ProfileSLMJudge,
		Scope: evals.ScopeOrg,
		Endpoint: evals.EvalEndpoint{
			BaseURL:   "https://judge.victim.example",
			Model:     "gpt-4o-mini",
			KeySource: evals.KeySourceInline,
			KeyRef:    "kr-victim",
		},
		Threshold: 0.5, SampleRate: 1.0, Enabled: true,
	}
}

func TestEvalProfilePatchRefusesAnotherOrgsID(t *testing.T) {
	srv := profileServer(t, victimProfile())
	mux := orgSessionMux("/api/eval/profiles/{id}", http.MethodPatch, srv.patchEvalProfile)

	req := httptest.NewRequest(http.MethodPatch, "/api/eval/profiles/ep-victim",
		strings.NewReader(`{"endpoint":{"base_url":"https://attacker.example","model":"m","key_source":"inline","key_ref":"kr-victim"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("PATCH on another org's profile returned %d, want 404: %s", rec.Code, rec.Body.String())
	}
	assertNoVictimDetail(t, rec.Body.String())
}

func TestEvalProfileDeleteRefusesAnotherOrgsID(t *testing.T) {
	store := newStubProfileStore(victimProfile())
	srv := newTestServer()
	srv.SetEvalProfiles(store)
	mux := orgSessionMux("/api/eval/profiles/{id}", http.MethodDelete, srv.deleteEvalProfile)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/eval/profiles/ep-victim", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE on another org's profile returned %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if _, ok := store.profiles["ep-victim"]; !ok {
		t.Error("the victim's profile was deleted by an admin of a different org")
	}
	assertNoVictimDetail(t, rec.Body.String())
}

// A POST cannot overwrite an existing profile by supplying its id, and cannot
// plant a profile inside another tenant by supplying their org.
func TestEvalProfileCreateCannotTargetAnotherOrgOrOverwriteByID(t *testing.T) {
	store := newStubProfileStore(victimProfile())
	srv := newTestServer()
	srv.SetEvalProfiles(store)
	mux := orgSessionMux("/api/eval/profiles", http.MethodPost, srv.createEvalProfile)

	body := `{
	  "id": "ep-victim",
	  "org_id": "` + victimOrg + `",
	  "name": "planted",
	  "kind": "heuristic_pii",
	  "scope": "org",
	  "sample_rate": 1.0,
	  "enabled": true
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/eval/profiles", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// The victim's row must be untouched.
	victim, ok := store.profiles["ep-victim"]
	if !ok {
		t.Fatal("the victim's profile disappeared")
	}
	if victim.Name != victimName {
		t.Errorf("a POST overwrote the victim's profile by supplying its id (name is now %q)", victim.Name)
	}

	// And whatever was created belongs to the caller, not to the victim.
	for id, p := range store.profiles {
		if id == "ep-victim" {
			continue
		}
		if p.OrgID != callerOrg {
			t.Errorf("created profile %q landed in org %q, want the caller's %q; "+
				"a body-supplied org would let one tenant plant a judge endpoint in another",
				id, p.OrgID, callerOrg)
		}
	}
}

func TestEvalProfileListShowsOnlyTheCallersOrg(t *testing.T) {
	srv := newTestServer()
	srv.SetEvalProfiles(newStubProfileStore(
		victimProfile(),
		&evals.EvalProfile{
			ID: "ep-mine", OrgID: callerOrg, Name: "mine", Kind: evals.ProfileHeuristicPII,
			Scope: evals.ScopeOrg, SampleRate: 1.0, Enabled: true,
		},
		&evals.EvalProfile{
			ID: "ep-wide", OrgID: "", Name: "seeded", Kind: evals.ProfileHeuristicPII,
			Scope: evals.ScopeOrg, SampleRate: 1.0, Enabled: true,
		},
	))
	mux := orgSessionMux("/api/eval/profiles", http.MethodGet, srv.listEvalProfiles)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/eval/profiles", nil))

	var payload struct {
		Profiles []evals.EvalProfile `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	var sawMine, sawWide bool
	for _, p := range payload.Profiles {
		switch p.ID {
		case "ep-victim":
			t.Errorf("the caller can read another org's profile, including its endpoint %q",
				p.Endpoint.BaseURL)
		case "ep-mine":
			sawMine = true
		case "ep-wide":
			sawWide = true
		}
	}
	if !sawMine {
		t.Error("the caller lost sight of their own profile")
	}
	if !sawWide {
		t.Error("the operator's cluster-wide seeded profile is no longer visible")
	}
	assertNoVictimDetail(t, rec.Body.String())
}

// --- benchmark schedules ---------------------------------------------------

func victimSchedule() core.BenchmarkSchedule {
	return core.BenchmarkSchedule{
		ID:             "sched-victim",
		OrgID:          victimOrg,
		Name:           victimName,
		Model:          "gpt-4o",
		CadenceSeconds: 3600,
		Enabled:        true,
	}
}

func TestScheduleGetRefusesAnotherOrgsID(t *testing.T) {
	stub := &scheduleStub{runs: []core.BenchmarkSchedule{victimSchedule()}}
	srv := newScheduleServer(stub)
	mux := orgSessionMux("/api/eval/benchmarks/schedules/{id}", http.MethodGet, srv.getBenchmarkSchedule)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/eval/benchmarks/schedules/sched-victim", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET on another org's schedule returned %d, want 404 (a 403 confirms the id exists): %s",
			rec.Code, rec.Body.String())
	}
	assertNoVictimDetail(t, rec.Body.String())
}

func TestScheduleDeleteRefusesAnotherOrgsID(t *testing.T) {
	stub := &scheduleStub{runs: []core.BenchmarkSchedule{victimSchedule()}}
	srv := newScheduleServer(stub)
	mux := orgSessionMux("/api/eval/benchmarks/schedules/{id}", http.MethodDelete, srv.deleteBenchmarkSchedule)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/eval/benchmarks/schedules/sched-victim", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE on another org's schedule returned %d, want 404", rec.Code)
	}
	if len(stub.runs) == 0 {
		t.Error("the victim's schedule was deleted by an admin of a different org")
	}
}

// Pause/resume mutate the row too, so they need the same gate as delete.
func TestSchedulePauseRefusesAnotherOrgsID(t *testing.T) {
	stub := &scheduleStub{runs: []core.BenchmarkSchedule{victimSchedule()}}
	srv := newScheduleServer(stub)
	mux := orgSessionMux("/api/eval/benchmarks/schedules/{id}/pause", http.MethodPost, srv.pauseBenchmarkSchedule)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/eval/benchmarks/schedules/sched-victim/pause", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("pausing another org's schedule returned %d, want 404", rec.Code)
	}
	if stub.lastEnabledID != "" {
		t.Errorf("the victim's schedule was toggled by an admin of a different org (id=%q)", stub.lastEnabledID)
	}
}

// A schedule with no org is legacy data from before attribution, not a
// cluster-wide row an operator installed. It belongs to the default org, so a
// caller in some other org must not reach it.
func TestScheduleWithNoOrgBelongsToTheDefaultOrgOnly(t *testing.T) {
	row := victimSchedule()
	row.OrgID = ""
	stub := &scheduleStub{runs: []core.BenchmarkSchedule{row}}
	srv := newScheduleServer(stub)
	mux := orgSessionMux("/api/eval/benchmarks/schedules/{id}", http.MethodGet, srv.getBenchmarkSchedule)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/eval/benchmarks/schedules/sched-victim", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("an unattributed schedule was served to org %q (%d); a blank org means "+
			"the default org, not every org", callerOrg, rec.Code)
	}
}

// assertNoVictimDetail checks that a refusal did not leak the object's contents.
// A 404 that still echoes the name, the endpoint, or the owning org tells the
// caller everything they were refused.
func assertNoVictimDetail(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{victimName, victimOrg, "judge.victim.example", "kr-victim"} {
		if strings.Contains(body, leak) {
			t.Errorf("the response leaked %q from another org's object: %s", leak, body)
		}
	}
}
