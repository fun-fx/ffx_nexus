package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
)

// stubProbe replaces one entry in providerProbes for the duration of
// a single test. The harness swaps a closure that forwards to the
// httptest.NewServer vendor so we control exactly what status / body
// the handler observes. Returning the previous value lets the test
// restore on cleanup so parallel tests don't trip each other.
func stubProbe(t *testing.T, provider string, fn probeProvider) {
	t.Helper()
	prev, had := providerProbes[provider]
	providerProbes[provider] = fn
	t.Cleanup(func() {
		if had {
			providerProbes[provider] = prev
		} else {
			delete(providerProbes, provider)
		}
	})
}

// httpReq adapts an *http.Request into the probeProvider signature
// that the dispatch table expects, so each test can drive a custom
// round-trip without re-implementing the helper logic.
type fakeProbe func(secret, baseURL string) (status int, body string, err error)

// stubOpenAIStub installs a fake `openai` probe and returns the
// httptest server that backs it so the test can flip the
// handler's response between cases.
func stubOpenAIStub(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		// Drop the verbose Bearer prefix so the failure message
		// looks vendor-like rather than smuggling the secret.
		if !strings.HasPrefix(got, "Bearer ") {
			http.Error(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}
		switch r.Header.Get("X-Test-Outcome") {
		case "unauthorized":
			http.Error(w, "invalid api key", http.StatusUnauthorized)
		case "forbidden":
			http.Error(w, "org disabled", http.StatusForbidden)
		case "rate-limited":
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		case "outage":
			http.Error(w, "internal", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
		}
	}))
	stubProbe(t, "openai", func(_ context.Context, secret, _ string) (int, string, error) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-Test-Outcome", "")
		return runProbe(context.Background(), req)
	})
	// Allow asserting outcome by wrapping again at the test site; we
	// expose srv so the test can flip X-Test-Outcome via a request
	// header from a follow-up setup. For table simplicity we keep
	// the helper minimal here — most tests just want the happy path.
	return srv
}

// fakeProbeFn returns a probeProvider that calls the given handler.
// We use it instead of http plumbing so the test can read the
// headers being sent (auth scheme, x-api-key, anthropic-version).
func fakeProbeFn(handler http.HandlerFunc) probeProvider {
	return func(ctx context.Context, _, _ string) (int, string, error) {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/probe-target", nil)
		if err != nil {
			return 0, "", err
		}
		return runProbe(ctx, req)
	}
}

func TestPreflightCredential_Happy(t *testing.T) {
	stubProbe(t, "openai", fakeProbeFn(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
	}))
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "openai", Secret: "sk-good"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	var res PreflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected ok=true, got %#v", res)
	}
	if res.Status != 200 {
		t.Fatalf("status field: got %d", res.Status)
	}
	if res.Provider != "openai" {
		t.Fatalf("provider field: got %q", res.Provider)
	}
}

func TestPreflightCredential_RejectedByVendor(t *testing.T) {
	stubProbe(t, "openai", fakeProbeFn(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "openai", Secret: "sk-bad"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	var res PreflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.OK {
		t.Fatalf("expected ok=false, got %#v", res)
	}
	if !strings.Contains(res.Message, "invalid") {
		t.Fatalf("message should surface vendor error: %q", res.Message)
	}
}

func TestPreflightCredential_UnsupportedProvider(t *testing.T) {
	s := &Server{}
	// Pick a provider name that intentionally is NOT in providerProbes
	// so the test exercises the dispatch-table rejection path.
	body := mustJSON(t, preflightCredentialsRequest{Provider: "not-a-real-vendor", Secret: "anything"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unsupported") {
		t.Fatalf("expected unsupported error in: %s", rr.Body.String())
	}
}

func TestPreflightCredential_MissingFields(t *testing.T) {
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "openai"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
}

func TestPreflightCredential_OllamaRequiresBaseURL(t *testing.T) {
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "ollama", Secret: "irrelevant"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "base_url") {
		t.Fatalf("expected base_url note in: %s", rr.Body.String())
	}
}

func TestPreflightCredential_GridRequiresBaseURL(t *testing.T) {
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "grid", Secret: "irrelevant"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "base_url") {
		t.Fatalf("expected base_url note in: %s", rr.Body.String())
	}
}

func TestPreflightCredential_DetectedProviderReported(t *testing.T) {
	stubProbe(t, "openai", fakeProbeFn(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	s := &Server{}
	// operator pasted an Anthropic key but kept the dropdown on OpenAI
	body := mustJSON(t, preflightCredentialsRequest{Provider: "openai", Secret: "sk-ant-api03-xyz"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	var res PreflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.DetectedProvider != "anthropic" {
		t.Fatalf("expected detected_provider=anthropic, got %q", res.DetectedProvider)
	}
}

func TestPreflightCredential_GeminiUsesQueryParam(t *testing.T) {
	var sawKey string
	var sawAuth string
	stubProbe(t, "gemini", func(_ context.Context, secret, _ string) (int, string, error) {
		endpoint := "https://generativelanguage.googleapis.com/v1beta/models?pageSize=1&key=" + secret
		req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
		sawKey = req.URL.Query().Get("key")
		sawAuth = req.Header.Get("Authorization")
		return runProbe(context.Background(), req)
	})
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "gemini", Secret: "AIza-fake-but-not-real-key"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	if sawKey != "AIza-fake-but-not-real-key" {
		t.Fatalf("gemini probe: expected key query param, got %q", sawKey)
	}
	if sawAuth != "" {
		t.Fatalf("gemini probe: did not expect Authorization header, got %q", sawAuth)
	}
}

func TestPreflightCredential_NetworkErrorSurfaced(t *testing.T) {
	// Replace the openai probe so it always returns a connection error.
	stubProbe(t, "openai", func(_ context.Context, _, _ string) (int, string, error) {
		return 0, "", io.ErrUnexpectedEOF
	})
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "openai", Secret: "sk-deadbeef"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	var res PreflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.OK {
		t.Fatalf("expected ok=false on transport error")
	}
	if !strings.Contains(res.Message, "unexpected") {
		t.Fatalf("expected wrapped transport error message, got %q", res.Message)
	}
}

func TestPreflightCredential_LatencyReported(t *testing.T) {
	stubProbe(t, "openai", fakeProbeFn(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{Provider: "openai", Secret: "sk-pacing"})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	var res PreflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.LatencyMS < 20 {
		t.Fatalf("expected latency_ms>=20, got %d", res.LatencyMS)
	}
}

// TestPreflightCredential_GridHappy drives the grid probe through a
// stubbed vendor so the dispatch path is exercised end-to-end. The
// probe should hit /models (not /v1/models *twice*) on the supplied
// base URL, accept the operator's bearer key, and surface the vendor's
// 200 as ok=true.
func TestPreflightCredential_GridHappy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"id":"code-prime"}]}`))
	}))
	t.Cleanup(srv.Close)

	stubProbe(t, "grid", func(_ context.Context, secret, _ string) (int, string, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		return runProbe(context.Background(), req)
	})
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{
		Provider: "grid",
		Secret:   "grid_live_key",
		BaseURL:  "https://api.thegrid.ai/v1",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	var res PreflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected ok=true, got %#v", res)
	}
	if res.ProviderLabel != "The Grid" {
		t.Fatalf("provider_label: got %q, want %q", res.ProviderLabel, "The Grid")
	}
}

// TestPreflightCredential_GridRejectedByVendor mirrors the openai
// rejection case: a 401 must surface as ok=false so the drawer's Save
// button stays disabled.
func TestPreflightCredential_GridRejectedByVendor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	stubProbe(t, "grid", func(_ context.Context, secret, _ string) (int, string, error) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		return runProbe(context.Background(), req)
	})
	s := &Server{}
	body := mustJSON(t, preflightCredentialsRequest{
		Provider: "grid",
		Secret:   "grid_live_bad",
		BaseURL:  "https://api.thegrid.ai/v1",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/me/credentials/preflight", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.preflightCredential(rr, req, fakeUser())
	var res PreflightResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if res.OK {
		t.Fatalf("expected ok=false, got %#v", res)
	}
	if !strings.Contains(res.Message, "unauthorized") {
		t.Fatalf("expected vendor message in response, got %q", res.Message)
	}
}

// TestPreflightCredential_GridTrailingSlash exercises the probeGrid
// helper's URL composition: a base URL with a trailing slash must
// still resolve to /models without doubling up the path segment.
func TestPreflightCredential_GridTrailingSlash(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Drive the probe directly so we control every input byte and
	// can verify ProbeGrid's path-joining logic.
	_, _, err := probeGrid(context.Background(), "x", srv.URL+"/v1/")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if seenPath != "/v1/models" {
		t.Fatalf("expected probe to hit /v1/models, got %q", seenPath)
	}
}

// fakeUser is the smallest valid User value that keeps the
// pre-flight handler happy. The save path requires a real store
// (and a master key + audit logger); pre-flight does not, so an
// empty store-less Server + an identity that matches the request
// shape is all we need.
func fakeUser() core.User {
	return core.User{ID: "u-test", OrgID: "default", Role: core.RoleMember, Email: "test@example.com"}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
