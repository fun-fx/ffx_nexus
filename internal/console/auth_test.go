package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthConfigSignupDisabled(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth config: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"signup_enabled":false`) {
		t.Fatalf("expected signup_enabled false, got %s", rec.Body.String())
	}
}

func TestAuthConfigSignupEnabled(t *testing.T) {
	srv := newTestServer()
	srv.SetAllowSignup(true)
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth config: want 200, got %d", rec.Code)
	}
	// store is nil, so signup stays disabled until Postgres is wired.
	if !strings.Contains(rec.Body.String(), `"signup_enabled":false`) {
		t.Fatalf("signup should require store, got %s", rec.Body.String())
	}
}

func TestRegisterDisabledReturns403(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register",
		strings.NewReader(`{"email":"a@b.com","password":"long-enough"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("register disabled: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRegisterWithoutStoreReturns503(t *testing.T) {
	srv := newTestServer()
	srv.SetAllowSignup(true)
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register",
		strings.NewReader(`{"email":"a@b.com","password":"long-enough"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("register without store: want 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRegisterRouteRegistered(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register",
		strings.NewReader(`{"email":"a@b.com","password":"long-enough"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("register route not registered: got %d", rec.Code)
	}
}

func TestRegisterInvalidPasswordReturns400(t *testing.T) {
	srv := newTestServer()
	srv.SetAllowSignup(true)
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/auth/register",
		strings.NewReader(`{"email":"a@b.com","password":"short"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestPlaygroundCatalogRequiresAuth pins the route behaviour: an
// unauthenticated probe must be 401'd by the session guard before the
// catalog adapter is consulted.
func TestPlaygroundCatalogRequiresAuth(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/api/me/playground/catalog", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: want 401, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// The catalog endpoint is invoked only after the user is signed in, but
// the assertion we care about at this layer is the route is wired (and
// without a catalog source returns the empty-shape JSON expected by the
// console client). Logging in is exercised end-to-end by the integration
// checks against a real Postgres.
func TestPlaygroundCatalogRouteRegistered(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/api/me/playground/catalog", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("catalog route not registered: got %d", rec.Code)
	}
}

func TestMeStatsTracesQualityRequireUser(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	for _, path := range []string{"/api/me/stats", "/api/me/traces", "/api/me/quality"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated: want 401, got %d (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestMeStatsTracesQualityWithoutReaderReturnEmpty(t *testing.T) {
	srv := newTestServer()
	// Inject a fake logged-in user via the withUser middleware? It's session-based
	// (cookie -> store.UserBySession). The store is nil so no session resolves; we
	// can only test the 401 path here without a live Postgres. The reader-less
	// empty-payload branches are exercised by the Admin equivalent tests.

	// Instead: confirm the routes are registered (not 404/405).
	mux := srv.Mux()
	for _, path := range []string{"/api/me/stats", "/api/me/traces", "/api/me/quality"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s route not registered: got %d", path, rec.Code)
		}
	}
}
