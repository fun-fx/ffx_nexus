package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests on the Gate are the canonical contract for /readyz
// across both Gateway and Worker pods. The worker mux wires
// ready.Handler() directly; the gateway mux wires it via
// gateway.NewMux. A drift between the two was caught in
// Phase D-1 review ("/readyz semantics differ between roles").
// These tests pin the gate-side behavior so both sides see
// the same response, with a pass/fail characteristic that
// can never differ across roles.

func TestReadyzSuccessWhenAllRequiredChecksPass(t *testing.T) {
	g := New()
	g.Set("migrations", true, true, "ok")
	g.Set("cipher", true, false, "optional")

	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when required checks pass; got code=%d", w.Code)
	}
}

func TestReadyzFailureWhenRequiredCheckFails(t *testing.T) {
	g := New()
	g.Set("migrations", false, true, "blocked")

	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	// Mirror: gateway and worker must both report not-ready
	// when a required check is missing, NOT 200. A 200 with
	// a missing dependency would route traffic to a pod that
	// cannot serve it.
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when a required check is failing; got code=%d", w.Code)
	}
}

func TestReadyzAcceptsBothGatewayAndWorkerPaths(t *testing.T) {
	// The worker pod's /readyz resolves to the same
	// Handler(). The gateway mux also exposes /readyz
	// through the same Handler. The path string is part
	// of the handler registration but Handler does not
	// the path check — the test simply verifies the
	// handler answers 200/503 consistently so
	// the Helm-chart-driven readinessProbe can rely on it
	// from either side.
	g := New()
	g.Set("migrations", true, true, "")

	for _, path := range []string{"/readyz", "/readyz?probe=true"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		g.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("path %q: expected 200; got %d", path, w.Code)
		}
	}
}
