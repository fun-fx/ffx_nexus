package rules

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// recordedReq captures one request the mock vendor receives. We
// record both the body and the auth-related headers so a single
// test can assert "the request matched the contract we promised
// to vendor-side devs" without spawning a sub-test per header.
type recordedReq struct {
	body   map[string]any
	apiKey string
	method string
	path   string
	raw    []byte
}

// startMockLangsmith stands up an httptest server that pretends
// to be api.smith.langchain.com. The handler is configured per
// test (success, 401, 422, 409, 5xx, slow). The recorder is the
// seam the tests use to assert the contract — without it, we'd
// be testing only that the function returned without crashing.
func startMockLangsmith(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *[]recordedReq) {
	t.Helper()
	var reqs []recordedReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		reqs = append(reqs, recordedReq{
			body:   parsed,
			apiKey: r.Header.Get("x-api-key"),
			method: r.Method,
			path:   r.URL.Path,
			raw:    body,
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

// TestCreateRuleSuccess asserts the happy path: a sane request
// body is POSTed to /api/v1/runs/rules, the response is parsed,
// and the parsed Rule includes the vendor-side id we will store
// on the EvalPlugin row.
func TestCreateRuleSuccess(t *testing.T) {
	srv, reqs := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != rulesPath {
			t.Errorf("path: got %q want %q", r.URL.Path, rulesPath)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "00000000-0000-0000-0000-000000000001",
			"display_name": "Nexus thegrid-judge webhook",
			"is_enabled": true
		}`))
	})
	c := New(srv.URL, "ls-test-key")
	rule, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "Nexus thegrid-judge webhook",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		IsEnabled:    true,
		SamplingRate: 1.0,
	}, "https://nexus.example.com/api/eval/plugins/thegrid-judge/webhook")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	if rule.ID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("Rule.ID: got %q want expected UUID", rule.ID)
	}
	if rule.DisplayName != "Nexus thegrid-judge webhook" {
		t.Errorf("Rule.DisplayName: got %q", rule.DisplayName)
	}
	if got := len(*reqs); got != 1 {
		t.Fatalf("vendor saw %d requests; want 1", got)
	}
	r := (*reqs)[0]
	if r.method != http.MethodPost {
		t.Errorf("method: got %q want POST", r.method)
	}
	if r.path != rulesPath {
		t.Errorf("path: got %q want %q", r.path, rulesPath)
	}
	if r.apiKey != "ls-test-key" {
		t.Errorf("x-api-key header: got %q want %q", r.apiKey, "ls-test-key")
	}
	if r.body["display_name"] != "Nexus thegrid-judge webhook" {
		t.Errorf("body.display_name: got %v", r.body["display_name"])
	}
	if r.body["session_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("body.session_id: got %v", r.body["session_id"])
	}
	if r.body["sampling_rate"] != 1.0 {
		t.Errorf("body.sampling_rate: got %v", r.body["sampling_rate"])
	}
	hooks, ok := r.body["webhooks"].([]any)
	if !ok || len(hooks) != 1 {
		t.Fatalf("body.webhooks: got %v want 1 entry", r.body["webhooks"])
	}
	hookURL := hooks[0].(map[string]any)["url"]
	if hookURL != "https://nexus.example.com/api/eval/plugins/thegrid-judge/webhook" {
		t.Errorf("webhook.url: got %v", hookURL)
	}
}

// TestCreateRuleRefusesEmptyAPIKey locks down the safety check
// at the top of CreateRule: the function must refuse to make a
// vendor call without an API key. A regression here would let a
// misconfigured admin REST endpoint send an unauthenticated
// request that the vendor would 401, but worse, it would log
// the body — which can include the proposed display_name and
// webhook URL but should never be sent with no auth.
func TestCreateRuleRefusesEmptyAPIKey(t *testing.T) {
	srv, reqs := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	c := New(srv.URL, " ")
	_, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "x",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		SamplingRate: 1.0,
	}, "https://n/x")
	if err == nil {
		t.Fatal("expected error on empty APIKey, got nil")
	}
	if !strings.Contains(err.Error(), "APIKey") {
		t.Errorf("error message should mention APIKey: %v", err)
	}
	if got := len(*reqs); got != 0 {
		t.Errorf("vendor must see 0 requests on empty key, saw %d", got)
	}
}

// TestCreateRuleValidation402Sentinel asserts that client-side
// validation failures (missing session_id, missing webhook URL,
// etc.) are surfaced as ErrValidation via AsValidation. The
// admin REST handler depends on this sentinel to return a 400
// instead of crashing through to 500.
func TestCreateRuleValidationSentinel(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not reach vendor for client-side validation failures")
	})
	c := New(srv.URL, "k")

	cases := []struct {
		name string
		req  CreateRuleRequest
		url  string
	}{
		{"missing session", CreateRuleRequest{DisplayName: "x", SamplingRate: 1.0}, "https://n/x"},
		{"missing display_name", CreateRuleRequest{SessionID: "s", SamplingRate: 1.0}, "https://n/x"},
		{"missing webhook URL", CreateRuleRequest{DisplayName: "x", SessionID: "s", SamplingRate: 1.0}, ""},
		{"sampling > 1", CreateRuleRequest{DisplayName: "x", SessionID: "s", SamplingRate: 2.0}, "https://n/x"},
		{"sampling <= 0", CreateRuleRequest{DisplayName: "x", SessionID: "s", SamplingRate: 0}, "https://n/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateRule(context.Background(), tc.req, tc.url)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if AsValidation(err) == nil {
				t.Errorf("AsValidation returned nil for client-side validation error: %v", err)
			}
		})
	}
}

// TestCreateRuleVendor401MapsToUnauthorized asserts the status
// mapping for 401 responses: the upstream-reported error text is
// preserved (operator needs to know which key was rejected) but
// the error chain contains ErrUnauthorized so admin REST
// handlers can branch without parsing strings.
func TestCreateRuleVendor401MapsToUnauthorized(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid api key"}`))
	})
	c := New(srv.URL, "k")
	_, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "x",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		SamplingRate: 1.0,
	}, "https://n/x")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("errors.Is(err, ErrUnauthorized) = false; err = %v", err)
	}
}

// TestCreateRuleVendor422MapsToInvalidRequest asserts the
// status mapping for 422 responses. A 422 means LangSmith
// accepted the request shape but rejected a field value
// (session_id malformed, sampling_rate out of range, etc.) —
// the typed ErrInvalidRequest helps the admin REST handler
// say "your inputs are bad" rather than "the vendor is broken".
func TestCreateRuleVendor422MapsToInvalidRequest(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":[{"loc":["body","session_id"]}]}`))
	})
	c := New(srv.URL, "k")
	_, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "x",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		SamplingRate: 1.0,
	}, "https://n/x")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("errors.Is(err, ErrInvalidRequest) = false; err = %v", err)
	}
}

// TestCreateRuleVendor409MapsToConflict asserts the
// idempotency contract: a duplicate display_name (a manual
// rule the operator made earlier) is surfaced as ErrConflict,
// letting the admin REST handler return a typed "already
// configured" UI state with PATCH-path guidance instead of a
// vague 5xx.
func TestCreateRuleVendor409MapsToConflict(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"display_name already exists"}`))
	})
	c := New(srv.URL, "k")
	_, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "dup",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		SamplingRate: 1.0,
	}, "https://n/x")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("errors.Is(err, ErrConflict) = false; err = %v", err)
	}
}

// TestCreateRuleVendor5xxMapsToUpstream asserts that
// Cloudflare-style 5xx pages (no body, default HTML) are
// still surfaced as ErrUpstream, not net/http's typed errors.
// Admin REST handlers use ErrUpstream to surface "vendor
// down" distinctly from "you typed bad input".
func TestCreateRuleVendor5xxMapsToUpstream(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>cloudflare</html>`))
	})
	c := New(srv.URL, "k")
	_, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "x",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		SamplingRate: 1.0,
	}, "https://n/x")
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("errors.Is(err, ErrUpstream) = false; err = %v", err)
	}
}

// TestCreateRuleRejectsMissingID asserts the integrity check
// at the end of CreateRule: a 2xx response without an id field
// is rejected so admin REST handlers don't save a rule id of
// "" into the plugins table (an "i can't delete this rule"
// footgun).
func TestCreateRuleRejectsMissingID(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"display_name":"x","is_enabled":true}`))
	})
	c := New(srv.URL, "k")
	_, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "x",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		SamplingRate: 1.0,
	}, "https://n/x")
	if err == nil {
		t.Fatal("expected error for missing id, got nil")
	}
	if !strings.Contains(err.Error(), "missing id") {
		t.Errorf("error should mention missing id: %v", err)
	}
}

// TestCreateRuleOverwritesFirstWebhookURL asserts the
// pre-emptive contract: even when the caller populated
// req.Webhooks[0].URL themselves (e.g. with a custom path), the
// client overwrites it with the webhookURL argument. Reasoning:
// the rules_client package cannot decide whether the system URL
// is correct; the admin REST handler can. Keeping the seam in
// handler-code avoids sticky shared-template formulas.
func TestCreateRuleOverwritesFirstWebhookURL(t *testing.T) {
	srv, reqs := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000099"}`))
	})
	c := New(srv.URL, "k")
	_, err := c.CreateRule(context.Background(), CreateRuleRequest{
		DisplayName:  "x",
		SessionID:    "11111111-1111-1111-1111-111111111111",
		SamplingRate: 1.0,
		Webhooks: []Webhook{
			{URL: "https://stale/wrong", Headers: map[string]string{"X-User": "alice"}},
		},
	}, "https://correct/nexus-hook")
	if err != nil {
		t.Fatalf("CreateRule: %v", err)
	}
	hooks := (*reqs)[0].body["webhooks"].([]any)
	hook := hooks[0].(map[string]any)
	if hook["url"] != "https://correct/nexus-hook" {
		t.Errorf("webhook.url: got %v want %q (overwritten by handler)",
			hook["url"], "https://correct/nexus-hook")
	}
	if hook["headers"].(map[string]any)["X-User"] != "alice" {
		t.Errorf("webhook.headers preserved: got %v", hook["headers"])
	}
}

// TestAppendWebhookSuccess asserts the upgrade-in-place
// path: an operator already had a LangSmith rule from before
// this feature shipped, so they don't recreate it — Nexus
// PATCHes the existing rule to add the webhook.
func TestAppendWebhookSuccess(t *testing.T) {
	srv, reqs := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method: got %q want PATCH", r.Method)
		}
		expectedPath := rulesPath + "/00000000-0000-0000-0000-000000000001"
		if r.URL.Path != expectedPath {
			t.Errorf("path: got %q want %q", r.URL.Path, expectedPath)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	c := New(srv.URL, "k")
	err := c.AppendWebhook(context.Background(),
		"00000000-0000-0000-0000-000000000001",
		Webhook{URL: "https://n/x", Headers: map[string]string{"X-Nexus": "1"}})
	if err != nil {
		t.Fatalf("AppendWebhook: %v", err)
	}
	r := (*reqs)[0]
	if r.body["webhooks"].([]any)[0].(map[string]any)["url"] != "https://n/x" {
		t.Errorf("body.webhooks[0].url: got %v", r.body["webhooks"])
	}
}

// TestAppendWebhookNotFound asserts the 404 mapping: a stale
// rule_id (operator deleted it manually) returns a typed error
// the admin REST handler can surface without parsing strings.
func TestAppendWebhookNotFound(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	})
	c := New(srv.URL, "k")
	err := c.AppendWebhook(context.Background(), "00000000-0000-0000-0000-0000000000ff",
		Webhook{URL: "https://n/x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

// TestAppendWebhookEmptyAPIKey asserts the same safety check
// as CreateRule: PATCH without a key is refused before any
// vendor call is made.
func TestAppendWebhookEmptyAPIKey(t *testing.T) {
	srv, _ := startMockLangsmith(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not reach vendor for empty key")
	})
	c := New(srv.URL, " ")
	err := c.AppendWebhook(context.Background(), "00000000-0000-0000-0000-000000000001",
		Webhook{URL: "https://n/x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestNewDefaultsEndpoint ensures the convenience constructor
// falls back to the public SaaS when given an empty Endpoint.
// Self-hosted deployments obviously override; the cloud
// fallback only matters when a config value is missing, which
// would be a deployment error.
func TestNewDefaultsEndpoint(t *testing.T) {
	c := New("", "k")
	if c.Endpoint != defaultCloudBase {
		t.Errorf("Endpoint: got %q want %q (cloud default)", c.Endpoint, defaultCloudBase)
	}
	c = New("https://self.host.example/", "k")
	if c.Endpoint != "https://self.host.example" {
		t.Errorf("Endpoint: trailing slash not trimmed: got %q", c.Endpoint)
	}
}

// TestSaaSEndpointConstant pins the public SaaS URL we ship
// against. The integration is the contract: an environment
// without NEXUS_LANGSMITH_ENDPOINT should send automation
// requests to api.smith.langchain.com. A regression here would
// silently send to a self-host cluster (or worse, a typo) and
// would only surface when an operator notices no rule appeared
// in their LangSmith tenant.
func TestSaaSEndpointConstant(t *testing.T) {
	if defaultCloudBase != "https://api.smith.langchain.com" {
		t.Errorf("defaultCloudBase: got %q want https://api.smith.langchain.com", defaultCloudBase)
	}
	if !strings.HasPrefix(rulesPath, "/api/v1/") {
		t.Errorf("rulesPath: got %q want /api/v1/…", rulesPath)
	}
}

// nopServer is a tiny helper used by tests that don't care
// about the response body — they only need to assert "vendor
// saw 0 requests during the failure path".
func nopServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}
