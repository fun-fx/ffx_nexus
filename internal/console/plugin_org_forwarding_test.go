package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
)

// The three by-name plugin routes — test, fire, automation — resolve a
// client-supplied {name} into a plugin whose stored vendor credential is then
// used to make an outbound call. Plugin names are unique per org, so the
// resolution must happen inside one tenant.
//
// The tenant check itself lives in the implementations (see
// cmd/nexus/plugin_tenancy_test.go), because that is where resolution and the
// vendor call happen. What the console is responsible for is supplying the org,
// and supplying the *session's* org rather than the client's claim about it.
// These tests pin that responsibility: they assert the value that arrives at the
// interface boundary.
//
// Without this, threading the parameter through would be untested at the layer
// that chooses its value, and passing "" — which every implementation treats as
// cluster-wide, i.e. "resolve anything" — would look identical to working code.

type orgRecordingTester struct{ gotOrg string }

func (s *orgRecordingTester) Test(_ context.Context, orgID, _ string) (Result, error) {
	s.gotOrg = orgID
	return Result{OK: true, Message: "probed"}, nil
}

type orgRecordingFirer struct{ gotOrg string }

func (s *orgRecordingFirer) FireManual(_ context.Context, orgID, _, _ string) (int, error) {
	s.gotOrg = orgID
	return 0, nil
}

func (s *orgRecordingFirer) FireScheduled(_ context.Context, orgID, _, _ string) (int, error) {
	s.gotOrg = orgID
	return 0, nil
}

type orgRecordingAutomator struct{ gotOrg string }

func (s *orgRecordingAutomator) CreateAutomationRule(_ context.Context, orgID, _, _ string) (AutomationRuleResult, error) {
	s.gotOrg = orgID
	return AutomationRuleResult{OK: true}, nil
}

// sessionOrgMux serves one plugin route with a session belonging to sessionOrg,
// so a request can also carry a conflicting X-Org-Id and the winner can be
// observed.
func sessionOrgMux(s *Server, pattern string, h func(http.ResponseWriter, *http.Request, core.User), sessionOrg string) http.Handler {
	r := chi.NewRouter()
	r.Post(pattern, func(w http.ResponseWriter, req *http.Request) {
		user := core.User{ID: "u1", Role: core.RoleAdmin, Email: "admin@example.com", OrgID: sessionOrg}
		ctx := context.WithValue(req.Context(), userCtxKey{}, user)
		h(w, req.WithContext(ctx), user)
	})
	return r
}

func TestPluginTestForwardsTheSessionOrg(t *testing.T) {
	srv := newTestServer()
	tester := &orgRecordingTester{}
	srv.SetPluginTester(tester)

	mux := sessionOrgMux(srv, "/api/eval/plugins/{name}/test", srv.pluginTest, "org-a")
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/judge/test", nil)
	// A hostile client claims to be another org. The session must win.
	req.Header.Set("X-Org-Id", "org-b")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if tester.gotOrg != "org-a" {
		t.Errorf("probe ran as org %q, want the session's org-a; "+
			"an X-Org-Id header must not choose whose vendor key is spent", tester.gotOrg)
	}
}

func TestPluginFireForwardsTheSessionOrg(t *testing.T) {
	for _, which := range []string{"", "scheduled"} {
		srv := newTestServer()
		firer := &orgRecordingFirer{}
		srv.SetPluginManualFirer(firer)

		mux := sessionOrgMux(srv, "/api/eval/plugins/{name}/fire", srv.pluginFireManual, "org-a")
		url := "/api/eval/plugins/judge/fire"
		if which != "" {
			url += "?which=" + which
		}
		req := httptest.NewRequest(http.MethodPost, url, nil)
		req.Header.Set("X-Org-Id", "org-b")
		mux.ServeHTTP(httptest.NewRecorder(), req)

		if firer.gotOrg != "org-a" {
			t.Errorf("which=%q fired as org %q, want org-a; firing dispatches traces "+
				"to the resolved plugin's vendor account", which, firer.gotOrg)
		}
	}
}

func TestAutomationRuleForwardsTheSessionOrg(t *testing.T) {
	srv := newTestServer()
	automator := &orgRecordingAutomator{}
	srv.SetLangSmithRuleCreator(automator)

	mux := sessionOrgMux(srv, "/api/eval/plugins/{name}/automation", srv.pluginCreateAutomationRule, "org-a")
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/judge/automation",
		strings.NewReader(`{"session_id":"11111111-1111-1111-1111-111111111111"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Org-Id", "org-b")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if automator.gotOrg != "org-a" {
		t.Errorf("rule created as org %q, want org-a", automator.gotOrg)
	}
}

// A session without an org must not degrade to "" — every implementation reads
// "" as cluster-wide and will resolve any plugin under that name. It has to fall
// back to the default org instead, which is a real tenant.
func TestPluginRoutesNeverForwardAnEmptyOrg(t *testing.T) {
	srv := newTestServer()
	tester := &orgRecordingTester{}
	srv.SetPluginTester(tester)

	mux := sessionOrgMux(srv, "/api/eval/plugins/{name}/test", srv.pluginTest, "")
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/judge/test", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if tester.gotOrg == "" {
		t.Error(`a session with no org forwarded "", which every resolver reads as ` +
			`cluster-wide — meaning "resolve any org's plugin with that name"`)
	}
	if tester.gotOrg != core.DefaultOrgID {
		t.Errorf("forwarded org %q, want the default org %q", tester.gotOrg, core.DefaultOrgID)
	}
}
