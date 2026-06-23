package console

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/core"
)

// newTestServer builds a console server with no datastores wired, which is
// enough to exercise the control-plane guard rails and route wiring.
func newTestServer() *Server {
	return NewServer(nil, nil, nil, slog.Default())
}

func TestRotateCredentialWithoutLoginReturns401(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	// Rotate is admin-only (v1.1); unauthenticated requests are rejected
	// by requireAdmin before the store guard fires.
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/abc/rotate",
		strings.NewReader(`{"secret":"new-secret"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("rotate without login: want 401, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRotateCredentialRouteRegistered(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	// A registered route returns 401 (admin guard fires first), not 404/405.
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/abc/rotate",
		strings.NewReader(`{"secret":"x"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Fatalf("rotate route not registered: got %d", rec.Code)
	}
}

func TestReloadCredentialsInvokesHook(t *testing.T) {
	srv := newTestServer()
	called := 0
	srv.SetCredentialReloader(func(context.Context) { called++ })

	srv.reloadCredentials(context.Background())
	if called != 1 {
		t.Fatalf("reloader hook should run once, ran %d times", called)
	}
}

func TestReloadCredentialsNoHookIsSafe(t *testing.T) {
	srv := newTestServer()
	// No reloader registered; must not panic.
	srv.reloadCredentials(context.Background())
}

// --- Audit log route (v1.1) ---

// /api/audit is admin-only, so an unauthenticated request should return 401.
func TestListAuditRequiresLogin(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("audit without session: want 401, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Without a Postgres store, even an authenticated admin request to /api/audit
// returns 503 — the route must be wired and the guard must fire in order.
func TestListAuditWithoutStoreReturns503(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	// Inject an admin user into the request context so requireAdmin passes.
	// We do this by pre-populating the context via a synthetic request, since
	// withUser only resolves via the session cookie / store.
	mux = wrapWithAdminCtx(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit without store: want 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// wrapWithAdminCtx wraps the mux so every request carries an admin user in
// its context, bypassing the withUser/session lookup that needs a real store.
func wrapWithAdminCtx(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), userCtxKey{}, core.User{
			ID:    "test-admin",
			OrgID: "default",
			Role:  core.RoleAdmin,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
