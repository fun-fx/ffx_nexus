package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// stubTester is a console.EvalPluginTester that always returns the
// canned Result/error pair supplied at construction time. The tests
// only care about how the handler shapes the HTTP response, not
// how a real probe would behave.
type stubTester struct {
	res Result
	err error
}

func (s *stubTester) Test(_ context.Context, _, _ string) (Result, error) {
	return s.res, s.err
}

// stubCollector captures Webhook payloads so the test can assert on
// whether the handler invoked the collector and what was queued.
type stubCollector struct {
	lastName  string
	lastBody  string
	returnErr error
}

func (s *stubCollector) Webhook(name string, body io.Reader) error {
	s.lastName = name
	if body != nil {
		buf := make([]byte, 4096)
		n, _ := body.Read(buf)
		s.lastBody = string(buf[:n])
	}
	return s.returnErr
}

// newTestMux wires just the two plugin handler endpoints so the
// tests do not need a fully authenticated session. The auth guard
// is exercised separately by the admin test suite.
func newTestMux(s *Server) http.Handler {
	r := chi.NewRouter()
	if s.pluginCollector != nil {
		r.Post("/api/eval/plugins/{name}/webhook", func(w http.ResponseWriter, r *http.Request) {
			s.pluginWebhook(w, r)
		})
	}
	if s.pluginTester != nil {
		r.Post("/api/eval/plugins/{name}/test", func(w http.ResponseWriter, r *http.Request) {
			s.pluginTest(w, r, core.User{ID: "u1", Role: core.RoleAdmin})
		})
	}
	// Always expose the 503 path so we can assert the typed
	// "not wired" message even when nothing else is configured.
	r.Post("/api/eval/plugins/{name}/test-disabled", func(w http.ResponseWriter, r *http.Request) {
		s.pluginTest(w, r, core.User{ID: "u1", Role: core.RoleAdmin})
	})
	return r
}

func TestPluginTest_ReturnsTypedShape(t *testing.T) {
	srv := newTestServer()
	srv.SetPluginTester(&stubTester{
		res: Result{OK: true, Message: "all good", LatencyMs: 11},
	})
	mux := newTestMux(srv)
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/test", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		OK        bool   `json:"ok"`
		Message   string `json:"message"`
		LatencyMs int64  `json:"latency_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || body.Message != "all good" || body.LatencyMs != 11 {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestPluginTest_FailureSurfacesTypedMessage(t *testing.T) {
	srv := newTestServer()
	srv.SetPluginTester(&stubTester{
		err: errors.New("endpoint acme.test returned HTTP 502 (request malformed)"),
	})
	mux := newTestMux(srv)
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/test", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// A failed probe must come back as 200 with ok=false. Answering
	// 5xx here let reverse proxies swap our JSON for their own error
	// page — Cloudflare returned branded "Error code 502" HTML, so the
	// operator saw an ingress complaint instead of the real reason.
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 so proxies cannot replace the body, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.OK {
		t.Errorf("OK should be false")
	}
	if !strings.Contains(body.Message, "HTTP 502") {
		t.Errorf("message should carry downstream detail, got %q", body.Message)
	}
}

func TestPluginTest_NoTesterReturnsTypedServiceUnavailable(t *testing.T) {
	srv := newTestServer() // pluginTester not set
	mux := newTestMux(srv)
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/test-disabled", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired") {
		t.Errorf("body should mention wiring state, got %q", rec.Body.String())
	}
}

func TestPluginWebhook_ReturnsAcceptedShape(t *testing.T) {
	srv := newTestServer()
	stub := &stubCollector{}
	srv.SetPluginCollector(stub)
	mux := newTestMux(srv)
	body := strings.NewReader(`{"name":"relevance","score":0.83,"label":"pass","comment":"ok","trace_id":"abc-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/langfuse-judge/webhook", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (%s)", rec.Code, rec.Body.String())
	}
	if stub.lastName != "langfuse-judge" {
		t.Errorf("expected collector got name %q", stub.lastName)
	}
	if !strings.Contains(stub.lastBody, "relevance") {
		t.Errorf("expected collector got body %q", stub.lastBody)
	}
	var b struct {
		OK       bool   `json:"ok"`
		Accepted int    `json:"accepted"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !b.OK || b.Accepted != 1 {
		t.Errorf("unexpected accepted shape: %+v", b)
	}
}

func TestPluginWebhook_RejectsWhenPluginModeIsPoll(t *testing.T) {
	srv := newTestServer()
	stub := &stubCollector{
		returnErr: errors.New("plugin \"poll-only\" is not in webhook collect mode"),
	}
	srv.SetPluginCollector(stub)
	mux := newTestMux(srv)
	body := strings.NewReader(`{"name":"x","score":1,"trace_id":"t"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/eval/plugins/poll-only/webhook", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	var b struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.OK || !strings.Contains(b.Message, "webhook collect mode") {
		t.Errorf("unexpected body: %+v", b)
	}
}

// pin the imported type so go vet does not complain about evalplugin
// when only admin tests change above.
var _ = evalplugin.Record{}
var _ = context.Background
