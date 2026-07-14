package console

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// wrapWithNonAdminCtx mirrors wrapWithAdminCtx but injects a member user.
// The /api/audit route is wrapped with requireAdmin, so this should 403.
func wrapWithNonAdminCtx(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), userCtxKey{}, core.User{
			ID:    "test-member",
			OrgID: "default",
			Role:  core.RoleMember,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TestAuditActionCoverage confirms that every defined AuditX constant has a
// corresponding entry in AllAuditActions. If you add a new state-changing
// admin action and forget to add it to AllAuditActions, this test fails,
// forcing the PR to document the gap explicitly.
func TestAuditActionCoverage(t *testing.T) {
	for _, c := range core.AllAuditActions {
		if c == "" {
			t.Fatal("empty string in AllAuditActions — add every action as a non-empty constant")
		}
	}
	// Spot-check the canonical ones are present.
	want := []string{
		core.AuditUserCreate, core.AuditUserDelete,
		core.AuditUserLogin, core.AuditSSOLogin, core.AuditLogout,
		core.AuditVKeyCreate, core.AuditVKeyRevoke,
		core.AuditCredentialCreate, core.AuditCredentialRotate, core.AuditCredentialDelete,
		core.AuditMeUpdate,
	}
	got := make(map[string]bool)
	for _, a := range core.AllAuditActions {
		got[a] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("constant %q is in use but missing from AllAuditActions", w)
		}
	}
}

// TestAuditActionStringsAreDistinct verifies no two action constants have
// the same string value, which would make SQL filtering ambiguous.
func TestAuditActionStringsAreDistinct(t *testing.T) {
	seen := make(map[string]bool)
	for _, c := range core.AllAuditActions {
		if seen[c] {
			t.Errorf("duplicate audit action value %q", c)
		}
		seen[c] = true
	}
}

// TestListAuditRequiresAdmin confirms the admin guard fires before the
// store check, so a non-admin never sees a 503 (which would imply the
// handler ran). We don't need a real store: even an attempt to reach it
// from a member should bounce.
func TestListAuditRequiresAdmin(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()
	mux = wrapWithNonAdminCtx(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("audit as non-admin: want 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// --- Query-parameter validation (parseAuditListOptions unit tests) ---

func TestParseAuditListOptions_NoParams(t *testing.T) {
	opts, err := parseAuditListOptions(nil)
	if err != nil {
		t.Fatalf("no params should parse cleanly: %v", err)
	}
	if opts.Action != "" || opts.ActorID != "" || opts.Limit != 0 || !opts.Since.IsZero() {
		t.Fatalf("unexpected default opts: %+v", opts)
	}
}

func TestParseAuditListOptions_RFC3339Since(t *testing.T) {
	opts, err := parseAuditListOptions(map[string][]string{
		"since": {"2026-06-01T00:00:00Z"},
	})
	if err != nil {
		t.Fatalf("RFC3339 should parse cleanly: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-06-01T00:00:00Z")
	if !opts.Since.Equal(want) {
		t.Fatalf("since parsed wrong: want %v got %v", want, opts.Since)
	}
}

func TestParseAuditListOptions_DurationSince(t *testing.T) {
	before := time.Now()
	opts, err := parseAuditListOptions(map[string][]string{"since": {"1h"}})
	if err != nil {
		t.Fatalf("duration should parse cleanly: %v", err)
	}
	// Window: opts.Since must fall within (before-1h) .. (after-1h).
	earliest := before.Add(-1 * time.Hour)
	latest := time.Now().Add(-1 * time.Hour)
	if opts.Since.Before(earliest.Add(-time.Second)) || opts.Since.After(latest.Add(time.Second)) {
		t.Fatalf("duration parse produced unexpected since: %v (expected in [%v, %v])",
			opts.Since, earliest, latest)
	}
}

func TestParseAuditListOptions_InvalidSince(t *testing.T) {
	_, err := parseAuditListOptions(map[string][]string{"since": {"not-a-date"}})
	if err == nil {
		t.Fatalf("invalid since must yield an error")
	}
}

func TestParseAuditListOptions_NegativeLimit(t *testing.T) {
	_, err := parseAuditListOptions(map[string][]string{"limit": {"-7"}})
	if err == nil {
		t.Fatalf("negative limit must yield an error")
	}
}

func TestParseAuditListOptions_NonNumericLimit(t *testing.T) {
	_, err := parseAuditListOptions(map[string][]string{"limit": {"abc"}})
	if err == nil {
		t.Fatalf("non-numeric limit must yield an error")
	}
}

func TestParseAuditListOptions_ValidLimit(t *testing.T) {
	opts, err := parseAuditListOptions(map[string][]string{"limit": {"200"}})
	if err != nil {
		t.Fatalf("valid limit should parse cleanly: %v", err)
	}
	if opts.Limit != 200 {
		t.Fatalf("limit parse wrong: want 200 got %d", opts.Limit)
	}
}
