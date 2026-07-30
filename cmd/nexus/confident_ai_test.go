package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
	"github.com/ffxnexus/nexus/internal/observability"
)

// captureRoundtrip returns a testserver that records the inbound
// request and replies with the requested status. Used by the
// per-vendor auth tests so we can see exactly what headers the
// adapter set.
func captureRoundtrip(t *testing.T, status int) (*httptest.Server, *atomic.Value) {
	t.Helper()
	got := &atomic.Value{}
	got.Store(http.Header{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Clone())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func targetFor(endpoint string, secretRef, keyRef string) external.Target {
	p := &evalplugin.Plugin{
		Metadata: evalplugin.PluginMetadata{Name: "probe"},
		Spec: evalplugin.PluginSpec{
			Service: evalplugin.ServiceSpec{
				Type:     evalplugin.ServiceConfidentAI,
				Endpoint: endpoint,
				Auth:     evalplugin.AuthSpec{SecretRef: secretRef, KeyRef: keyRef},
			},
		},
	}
	return external.Target{
		Endpoint: endpoint,
		Auth:     external.Credentials{Values: []string{"public-x", "secret-y"}},
		Plugin:   p,
		Trace:    observability.Trace{TraceID: "tr-1"},
	}
}

func TestConfidentAITransmit_BasicAuthHeaders(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	err := confidentAITransmit(context.Background(),
		targetFor(srv.URL, "", "public_x|secret_y"), nil)
	if err != nil {
		t.Fatalf("transmit: %v", err)
	}
	h := got.Load().(http.Header)
	auth := h.Get("Authorization")
	if auth == "" {
		t.Fatalf("missing Authorization header on confident_ai; want Basic …, got %v", h)
	}
	// Basic decoded must contain our pair.
	user, pass, ok := decodeBasicAuth(auth)
	if !ok || user != "public-x" || pass != "secret-y" {
		t.Errorf("basic auth = %q, want Basic public-x:secret-y", auth)
	}
}

func TestArizePhoenixTransmit_BearerAuth(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	tgt := external.Target{
		Endpoint: srv.URL,
		Auth:     external.Credentials{Values: []string{"ai-key-1"}},
		Plugin: &evalplugin.Plugin{
			Metadata: evalplugin.PluginMetadata{Name: "probe"},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{Type: evalplugin.ServiceArizePhoenix},
			},
		},
		Trace: observability.Trace{TraceID: "tr-2"},
	}
	if err := arizePhoenixTransmit(context.Background(), tgt, nil); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	h := got.Load().(http.Header)
	if got := h.Get("Authorization"); got != "Bearer ai-key-1" {
		t.Errorf("Bearer header = %q", got)
	}
	if ua := h.Get("User-Agent"); ua == "" {
		t.Error("User-Agent should be set to identify the SDK origin")
	}
}

func TestArizePhoenixTransmit_PairAuthWinsOverBearer(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	tgt := external.Target{
		Endpoint: srv.URL,
		Auth:     external.Credentials{Values: []string{"space-1", "key-2"}},
		Plugin: &evalplugin.Plugin{
			Metadata: evalplugin.PluginMetadata{Name: "probe"},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{Type: evalplugin.ServiceArizePhoenix},
			},
		},
		Trace: observability.Trace{TraceID: "tr-3"},
	}
	if err := arizePhoenixTransmit(context.Background(), tgt, nil); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	h := got.Load().(http.Header)
	auth := h.Get("Authorization")
	user, pass, ok := decodeBasicAuth(auth)
	if !ok || user != "space-1" || pass != "key-2" {
		t.Errorf("pair shape preferred, got Authorization=%q (want Basic space-1:key-2)", auth)
	}
}

func TestConfidentAIAuthMissingReturnsError(t *testing.T) {
	srv, _ := captureRoundtrip(t, http.StatusOK)
	tgt := external.Target{
		Endpoint: srv.URL,
		Auth:     external.Credentials{},
		Plugin: &evalplugin.Plugin{
			Metadata: evalplugin.PluginMetadata{Name: "probe"},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{
					Type:     evalplugin.ServiceConfidentAI,
					Endpoint: srv.URL,
				},
			},
		},
		Trace: observability.Trace{TraceID: "tr-4"},
	}
	err := confidentAITransmit(context.Background(), tgt, nil)
	if err == nil {
		t.Fatal("expected auth error when no credentials are wired")
	}
	if got := err.Error(); !contains(got, "needs a single API key") {
		t.Errorf("error message should explain the auth requirement, got %q", got)
	}
}

func TestBraintrustTransmit_BearerHeader(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	tgt := external.Target{
		Endpoint: srv.URL,
		Auth:     external.Credentials{Values: []string{"bt-key"}},
		Plugin: &evalplugin.Plugin{
			Metadata: evalplugin.PluginMetadata{Name: "probe"},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{Type: evalplugin.ServiceBraintrust},
			},
		},
		Trace: observability.Trace{TraceID: "tr-5"},
	}
	if err := braintrustTransmit(context.Background(), tgt, nil); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	if got := got.Load().(http.Header).Get("Authorization"); got != "Bearer bt-key" {
		t.Errorf("Authorization = %q, want Bearer bt-key", got)
	}
}

func TestDatadogTransmit_DDAPIKeyHeader(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	tgt := external.Target{
		Endpoint: srv.URL,
		Auth:     external.Credentials{Values: []string{"dd-key"}},
		Plugin: &evalplugin.Plugin{
			Metadata: evalplugin.PluginMetadata{Name: "probe"},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{Type: evalplugin.ServiceDatadog},
			},
		},
		Trace: observability.Trace{TraceID: "tr-6"},
	}
	if err := datadogTransmit(context.Background(), tgt, map[string]any{"trace_id": "abc"}); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	if got := got.Load().(http.Header).Get("DD-API-KEY"); got != "dd-key" {
		t.Errorf("DD-API-KEY = %q, want dd-key", got)
	}
}

// Helpers ------------------------------------------------------------------

func decodeBasicAuth(s string) (string, string, bool) {
	const prefix = "Basic "
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", "", false
	}
	// delegate to net/http's stdlib helper via the test rig.
	req, err := http.NewRequest("GET", "http://x/", nil)
	if err != nil {
		return "", "", false
	}
	req.Header.Set("Authorization", s)
	user, pass, ok := req.BasicAuth()
	return user, pass, ok
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
