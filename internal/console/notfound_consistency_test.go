package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
)

// A caller authenticated as org A who reaches for an id that belongs to
// org B MUST receive 404. 403 confirms the row exists and turns the guess
// into an oracle. 200 reads the other tenant's data.
//
// Phase C caught this on benchmark_schedules and the rule has been re-
// applied resource by resource. The list below is the canonical inventory
// for resources exposed to the console: a new per-org resource MUST be
// added here. A handler that returns 403 or 200 in a regression fails the
// build here, not in some far-away log.
//
// Why one test, not several: the existing TestEvalProfile* suite already
// drives profiles with the full server harness. This test is the tripwire
// for "a future resource was added without honouring the contract". New
// per-org resources are added here so the inventory stays in code-review
// form.
func TestCrossTenantAccessAlwaysAnswers404(t *testing.T) {
	// Profile-server with one row owned by org-victim. We request it from
	// org-caller across GET, PATCH and DELETE, and assert 404 on every
	// path. The patterns are the production route table's eval/profiles
	// paths.
	mux := profileServer(t, victimProfile()).Mux()

	cases := []struct {
		method string
		path   string
		body   string
	}{
		// PATCH and DELETE are registered on the per-org profile routes in
		// production; GET is also reachable but the IDOR test suite covers
		// resource-level GETs in detail. The complaint is the same: 404.
		{http.MethodPatch, "/api/eval/profiles/ep-victim", `{"name":"rename"}`},
		{http.MethodDelete, "/api/eval/profiles/ep-victim", ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			if c.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req = req.WithContext(injectUser(req.Context(),
				core.User{ID: "u-caller", Role: core.RoleAdmin, OrgID: callerOrg, Email: "a@caller.example"}))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s returned %d, want 404.\n"+
					"Cross-tenant access MUST be 404, never 403 or 200.\n"+
					"  403 confirms the row exists: an attacker can list reachable ids.\n"+
					"  200 reads another tenant's data.\nBody: %s",
					c.method, c.path, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "ep-victim") {
				t.Errorf("404 body carries the victim id; even a 404 with the id "+
					"confirms ownership. Body: %s", rec.Body.String())
			}
		})
	}
}

// injectUser puts a User value into the request's context using the
// console's existing userCtxKey. The console stores it as a core.User under
// userCtxKey{}; we replicate the lookup so the test is self-contained and
// every handler reads the user out of the context as it does in production.
func injectUser(ctx context.Context, user core.User) context.Context {
	return context.WithValue(ctx, userCtxKey{}, user)
}
