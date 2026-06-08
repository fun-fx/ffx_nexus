package console

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer builds a console server with no datastores wired, which is
// enough to exercise the control-plane guard rails and route wiring.
func newTestServer() *Server {
	return NewServer(nil, nil, nil, slog.Default())
}

func TestRotateCredentialWithoutStoreReturns503(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodPost, "/api/credentials/abc/rotate",
		strings.NewReader(`{"secret":"new-secret"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rotate without store: want 503, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRotateCredentialRouteRegistered(t *testing.T) {
	srv := newTestServer()
	mux := srv.Mux()

	// A registered route returns 503 (store disabled), not 404/405.
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
