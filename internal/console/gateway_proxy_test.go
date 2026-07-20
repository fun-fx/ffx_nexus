package console

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoopbackGatewayURL(t *testing.T) {
	tests := map[string]string{
		":8080":       "http://127.0.0.1:8080",
		"0.0.0.0:9090": "http://127.0.0.1:9090",
		"127.0.0.1:8080": "http://127.0.0.1:8080",
	}
	for in, want := range tests {
		if got := loopbackGatewayURL(in); got != want {
			t.Fatalf("loopbackGatewayURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConsoleGatewayProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	srv := newTestServer()
	srv.SetGatewayProxy(upstream.Listener.Addr().String())
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != `{"data":[]}` {
		t.Fatalf("body = %q", body)
	}
}

func TestAuthConfigGatewayURL(t *testing.T) {
	srv := newTestServer()
	srv.SetPublicGatewayURL("https://api.nexus.ffx.ai")
	mux := srv.Mux()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/config", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"gateway_url"`) || !strings.Contains(rec.Body.String(), `"https://api.nexus.ffx.ai"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}
