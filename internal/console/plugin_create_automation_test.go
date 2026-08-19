package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubLangSmithRuleCreator is the test seam for LangSmithRuleCreator.
// Each field is recorded verbatim so a single fixture per test case
// reads as a "what was wired, what came back" table.
type stubLangSmithRuleCreator struct {
	gotLast  string
	gotSessL string
	resp     AutomationRuleResult
	err      error
	calls    int
}

func (s *stubLangSmithRuleCreator) CreateAutomationRule(_ context.Context, _, pluginName, sessionID string) (AutomationRuleResult, error) {
	s.calls++
	s.gotLast = pluginName
	s.gotSessL = sessionID
	return s.resp, s.err
}

// newAutomationMux wires the /api/eval/plugins/{name}/automation
// route through the same fixtures the rest of the admin REST tests
// use: newTestServer for the wired dependencies and wrapWithAdminCtx
// for the auth bypass. The route is registered by the Server itself
// (per cmd/nexus/server.go's admin router block) so the test only
// has to attach the rule creator — no manual mux wiring.
func newAutomationMux(t *testing.T, creator LangSmithRuleCreator) *Server {
	t.Helper()
	srv := newTestServer()
	srv.SetLangSmithRuleCreator(creator)
	srv.langsmithRuleCreator = creator
	return srv
}

// TestAutomationRule_NotWiredReturns503 pins the nil-interface
// contract: when SetLangSmithRuleCreator was never called the
// handler responds with a typed envelope (HTTP 503 + the
// "not wired" message) rather than crashing. The React UI relies
// on 503-but-typed so the button stays enabled but the message
// shows the operator what to do.
func TestAutomationRule_NotWiredReturns503(t *testing.T) {
	srv := newTestServer()
	mux := wrapWithAdminCtx(srv.Mux())
	req := adminRequest(http.MethodPost, "/api/eval/plugins/thegrid-judge/automation", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d want 503; body=%s", rec.Code, rec.Body.String())
	}
	var env AutomationRuleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.OK {
		t.Errorf("OK: got true want false")
	}
	if !strings.Contains(env.Message, "not wired") {
		t.Errorf("Message should mention 'not wired': %q", env.Message)
	}
}

// TestAutomationRule_HappyPath pins the contract that 200 OK +
// typed body is what the React client sees when LangSmith
// returned a valid rule id. The body asserts the resolution path
// (pluginName + sessionID flowed through, no body echo) so a
// regression in either input handle is loud.
func TestAutomationRule_HappyPath(t *testing.T) {
	stub := &stubLangSmithRuleCreator{
		resp: AutomationRuleResult{
			OK:         true,
			RuleID:     "00000000-0000-0000-0000-000000000abc",
			WebhookURL: "https://nexus.example.com/api/eval/plugins/thegrid-judge/webhook",
			Message:    "LangSmith automation rule created for plugin \"thegrid-judge\"",
		},
	}
	srv := newAutomationMux(t, stub)
	mux := wrapWithAdminCtx(srv.Mux())
	body := bytes.NewReader([]byte(`{"session_id":"11111111-1111-1111-1111-111111111111"}`))
	req := adminRequest(http.MethodPost,
		"/api/eval/plugins/thegrid-judge/automation", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200: body=%s", rec.Code, rec.Body.String())
	}
	var env AutomationRuleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.OK {
		t.Errorf("OK: got false; message=%q", env.Message)
	}
	if env.RuleID != "00000000-0000-0000-0000-000000000abc" {
		t.Errorf("RuleID: got %q", env.RuleID)
	}
	if stub.calls != 1 {
		t.Errorf("calls: got %d want 1", stub.calls)
	}
	if stub.gotLast != "thegrid-judge" {
		t.Errorf("pluginName: got %q want %q", stub.gotLast, "thegrid-judge")
	}
	if stub.gotSessL != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("sessionID: got %q want %q", stub.gotSessL, "11111111-1111-1111-1111-111111111111")
	}
}

// TestAutomationRule_AlreadyConfiguredMapsToTypedEnvelope is the
// most important typed-envelope test: ErrConflict from the
// LangSmith client maps to AlreadyConfigured=true on the wire so
// the React client can swap to the "you already configured this"
// branch of the help text without parsing strings. The handler
// returns 200 OK + ok:false — a vendor-side conflict is not an
// HTTP failure.
func TestAutomationRule_AlreadyConfiguredMapsToTypedEnvelope(t *testing.T) {
	stub := &stubLangSmithRuleCreator{
		resp: AutomationRuleResult{
			AlreadyConfigured: true,
			WebhookURL:        "https://nexus.example.com/api/eval/plugins/thegrid-judge/webhook",
			Message:           "LangSmith already has a rule with this display name in this project. Open the project and verify the webhook, or rename the plugin.",
		},
		err: errors.New("rule conflict"),
	}
	srv := newAutomationMux(t, stub)
	mux := wrapWithAdminCtx(srv.Mux())
	body := bytes.NewReader([]byte(`{"session_id":"abcdef00-aaaa-bbbb-cccc-dddddddddddd"}`))
	req := adminRequest(http.MethodPost,
		"/api/eval/plugins/thegrid-judge/automation", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var env AutomationRuleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.OK {
		t.Errorf("OK: got true want false on conflict")
	}
	if !env.AlreadyConfigured {
		t.Errorf("AlreadyConfigured: got false want true")
	}
	if env.WebhookURL == "" {
		t.Errorf("WebhookURL should be present so UI can show what was advertised")
	}
	if !strings.Contains(env.Message, "display name") {
		t.Errorf("Message should mention display_name: %q", env.Message)
	}
}

// TestAutomationRule_UnauthorizedMapsToTypedEnvelope pins the
// 401 path: LangSmith rejecting the API key surfaces in the UI
// with a short message rather than a stack trace. The body
// envelope must contain a recognizable phrase so the React
// client can grep the message and surface a "rotate the key"
// hint.
func TestAutomationRule_UnauthorizedMapsToTypedEnvelope(t *testing.T) {
	stub := &stubLangSmithRuleCreator{
		resp: AutomationRuleResult{
			Message: "LangSmith rejected the API key. Verify the key in the Plugin Keys panel.",
		},
		err: errors.New("LangSmith rejected the API key. Verify the key in the Plugin Keys panel and that the key belongs to the same workspace as the project."),
	}
	srv := newAutomationMux(t, stub)
	mux := wrapWithAdminCtx(srv.Mux())
	body := bytes.NewReader([]byte(`{"session_id":"abcdef00-aaaa-bbbb-cccc-dddddddddddd"}`))
	req := adminRequest(http.MethodPost,
		"/api/eval/plugins/thegrid-judge/automation", body)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
	var env AutomationRuleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.OK {
		t.Errorf("OK: got true want false on auth rejection")
	}
	if !strings.Contains(env.Message, "rejected the API key") {
		t.Errorf("Message should mention key rejection: %q", env.Message)
	}
}

// TestAutomationRule_ValidationErrorReturnsTypedEnvelope pins
// the input-validation path: a missing session_id (the most
// common operator mistake) is surfaced as 200 OK + ok:false so
// the typed envelope survives reverse proxies (the same logic
// PR #197 documented for the test-send route).
func TestAutomationRule_ValidationErrorReturnsTypedEnvelope(t *testing.T) {
	// For validation failures (missing session_id) the stub has no
	// WebhookURL because it never reached the URL computation. The
	// test therefore only asserts typed-message presence — the
	// typed envelope survives reverse proxies (the same logic PR
	// #197 documented for the test-send route).
	stub := &stubLangSmithRuleCreator{
		err: errors.New("session_id is required (paste the LangSmith project UUID from Settings → Projects)"),
	}
	srv := newAutomationMux(t, stub)
	mux := wrapWithAdminCtx(srv.Mux())
	// Empty body — no session_id field present.
	req := adminRequest(http.MethodPost,
		"/api/eval/plugins/thegrid-judge/automation", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
	var env AutomationRuleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.OK {
		t.Errorf("OK: got true want false on missing session_id")
	}
	if !strings.Contains(env.Message, "session_id") {
		t.Errorf("Message should mention session_id: %q", env.Message)
	}
}

// adminRequest is the helper that builds an authenticated admin
// request. The auth bypass in wrapWithAdminCtx reads the user
// from the context, so we attach the same value the admin path
// would have populated. NewServer logic looks for a session
// cookie by default; we sidestep that.
func adminRequest(method, path string, body *bytes.Reader) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, body)
	}
	return r
}
