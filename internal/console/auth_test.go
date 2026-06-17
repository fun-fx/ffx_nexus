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
