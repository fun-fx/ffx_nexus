package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/egress"
	"github.com/ffxnexus/nexus/internal/evals"
)

// The console must refuse a judge endpoint the guard would not dial, at the
// moment the admin submits it.
//
// The guard stops the request either way, so what these tests protect is the
// admin's ability to understand the refusal. A profile that saves and then never
// produces a score looks identical to a wrong key, a sample rate of zero, or a
// worker that is not running — and nobody debugging that arrives at "the
// destination address policy refused it".

func profileJSON(t *testing.T, baseURL string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":  "judge",
		"kind":  string(evals.ProfileSLMJudge),
		"scope": string(evals.ScopeOrg),
		"endpoint": map[string]any{
			"base_url":   baseURL,
			"model":      "judge-model",
			"key_source": string(evals.KeySourceInline),
			"key_ref":    "kr-1",
		},
		"sample_rate": 1.0,
		"enabled":     true,
		"threshold":   0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func postProfile(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv := profileServer(t)
	mux := orgSessionMux("/api/eval/profiles", http.MethodPost, srv.createEvalProfile)
	req := httptest.NewRequest(http.MethodPost, "/api/eval/profiles", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreatingAProfileWithAnInternalEndpointIsRefused(t *testing.T) {
	egress.TestingStrict(t) // this test is about the destination policy

	for _, target := range []struct{ name, base string }{
		{"cloud instance metadata", "http://169.254.169.254/latest/meta-data/"},
		{"the pod's own console port", "http://127.0.0.1:8081/v1"},
		{"a ClusterIP", "http://10.4.1.9:8123/v1"},
		{"no scheme", "10.4.1.9:8123"},
		{"a file URL", "file:///etc/passwd"},
		{"credentials in the URL", "https://u:p@judge.vendor.example/v1"},
	} {
		rec := postProfile(t, profileJSON(t, target.base))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST returned %d, want 400. The profile was accepted and will "+
				"fail silently at dispatch time instead of telling the admin why.\nbody: %s",
				target.name, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "base_url") {
			t.Errorf("%s: the error does not name the offending field: %s",
				target.name, rec.Body.String())
		}
		// The refusal must not echo a credential that was in the URL.
		if strings.Contains(rec.Body.String(), ":p@") {
			t.Errorf("%s: the error echoed the URL's credential: %s", target.name, rec.Body.String())
		}
	}
}

// A public endpoint still saves, or the test above would pass on a handler that
// rejects every endpoint.
func TestCreatingAProfileWithAPublicEndpointSucceeds(t *testing.T) {
	egress.TestingStrict(t)

	rec := postProfile(t, profileJSON(t, "https://judge.vendor.example/v1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a public endpoint was refused with %d: %s", rec.Code, rec.Body.String())
	}
}

// A builtin heuristic has no reachable endpoint and must not be held to a URL
// rule that does not apply to it.
func TestABuiltinHeuristicProfileNeedsNoReachableEndpoint(t *testing.T) {
	egress.TestingStrict(t)

	body, err := json.Marshal(map[string]any{
		"name":        "pii",
		"kind":        string(evals.ProfileHeuristicPII),
		"scope":       string(evals.ScopeOrg),
		"endpoint":    map[string]any{"key_source": string(evals.KeySourceBuiltin)},
		"sample_rate": 1.0,
		"enabled":     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := postProfile(t, string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("a builtin heuristic profile was refused with %d: %s", rec.Code, rec.Body.String())
	}
}

// PATCH is the other way in. Checking only on create would let an admin save a
// harmless URL and then move it.
func TestPatchingAProfileToAnInternalEndpointIsRefused(t *testing.T) {
	egress.TestingStrict(t)

	own := victimProfile()
	own.OrgID = callerOrg // the caller's own profile, so authorization is not what refuses
	own.Endpoint.BaseURL = "https://judge.vendor.example/v1"

	srv := profileServer(t, own)
	mux := orgSessionMux("/api/eval/profiles/{id}", http.MethodPatch, srv.patchEvalProfile)

	req := httptest.NewRequest(http.MethodPatch, "/api/eval/profiles/"+own.ID,
		strings.NewReader(`{"endpoint":{"base_url":"http://169.254.169.254/latest/meta-data/",`+
			`"model":"judge-model","key_source":"inline","key_ref":"kr-1"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH pointing at the metadata service returned %d, want 400: %s",
			rec.Code, rec.Body.String())
	}
}
