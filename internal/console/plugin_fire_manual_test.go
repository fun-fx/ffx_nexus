package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// stubManualFirer mocks PluginManualFirer for the route tests.
// The point of these tests is to assert the handler routes the
// request, decodes the body, and shapes the response — not to
// verify what the real scheduler does. Count + err lets one struct
// serve both happy-path and error-path assertions.
type stubManualFirer struct {
	name    string
	trigger string
	count   int
	err     error
}

func (s *stubManualFirer) FireManual(_ context.Context, name, trigger string) (int, error) {
	s.name = name
	s.trigger = trigger
	return s.count, s.err
}

// newFireMux wires the manual-fire endpoint so the test surface
// matches the existing pattern in eval_plugins_test.go. Auth is
// not exercised here; the live admin path is what requireAdmin
// runs in production.
func newFireMux(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/api/eval/plugins/{name}/fire", func(w http.ResponseWriter, r *http.Request) {
		s.pluginFireManual(w, r, core.User{ID: "u1", Role: core.RoleAdmin, Email: "admin@example.com"})
	})
	return r
}

// TestPluginFireManual_DrainsCount makes sure the admin REST's
// successful path round-trips the firer's count.
func TestPluginFireManual_DrainsCount(t *testing.T) {
	s := newTestServer()
	firer := &stubManualFirer{count: 42}
	s.SetPluginManualFirer(firer)
	mux := newFireMux(s)

	body, _ := json.Marshal(map[string]string{"trigger": "weekly-smoke-run"})
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/fire",
		strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Count   int    `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK {
		t.Errorf("ok = false; want true (count=%d)", resp.Count)
	}
	if resp.Count != 42 {
		t.Errorf("count = %d; want 42", resp.Count)
	}
	if firer.name != "langfuse-judge" {
		t.Errorf("firer called with %q; want langfuse-judge", firer.name)
	}
	if firer.trigger != "weekly-smoke-run" {
		t.Errorf("firer trigger = %q; want weekly-smoke-run", firer.trigger)
	}
}

// TestPluginFireManual_DefaultTriggerFromEmail: when the body omits
// a trigger, the handler stamps with the admin's email and the wall
// clock. This is what operators correlate from log lines back to
// their actions.
func TestPluginFireManual_DefaultTriggerFromEmail(t *testing.T) {
	s := newTestServer()
	firer := &stubManualFirer{count: 0}
	s.SetPluginManualFirer(firer)
	mux := newFireMux(s)

	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/fire", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(firer.trigger, "admin@example.com") {
		t.Errorf("default trigger should contain admin's email; got %q", firer.trigger)
	}
	if !strings.Contains(firer.trigger, "@") {
		t.Errorf("default trigger should be <email>@<RFC3339>; got %q", firer.trigger)
	}
}

// TestPluginFireManual_ErrorFriendly: a firer error must surface
// as 200 with `ok:false` and a clear message. Cloudflare will
// rewrite 5xx into HTML; 200 keeps the JSON body intact.
func TestPluginFireManual_ErrorFriendly(t *testing.T) {
	s := newTestServer()
	firer := &stubManualFirer{err: errors.New("manual-fire unavailable right now")}
	s.SetPluginManualFirer(firer)
	mux := newFireMux(s)

	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/fire", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (a typed error response is data, not transport)", rec.Code)
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		Count   int    `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OK {
		t.Errorf("ok = true; want false on firer error")
	}
	if !strings.Contains(resp.Message, "manual-fire unavailable") {
		t.Errorf("message = %q; want it to surface the firer error", resp.Message)
	}
}

// TestPluginFireManual_Unwired503: without a firer attached the
// route must answer 503, not 404 — the button should remain in the
// UI with a "service unavailable" tooltip rather than disappear.
func TestPluginFireManual_Unwired503(t *testing.T) {
	s := newTestServer()
	mux := newFireMux(s)
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/fire", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
}

// silenceUnused keeps the evalplugin import live even though tests
// don't construct plugins explicitly.
var _ = evalplugin.Plugin{}
