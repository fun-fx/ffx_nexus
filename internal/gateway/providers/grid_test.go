package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Covers the Grid provider shape: stable provider name for routing, the
// instrument catalog advertised at /v1/models, and the right base URL.
func TestGridProviderShape(t *testing.T) {
	g := NewGrid("grid-test", 0)
	if g.Name() != "grid" {
		t.Fatalf("want name=grid, got %q", g.Name())
	}
	models := g.Models()
	if len(models) != 9 {
		t.Fatalf("Grid should expose 9 instruments (3 tiers × 3 standards); got %d (%v)", len(models), models)
	}
	if !strings.HasPrefix(g.OpenAI.baseURL, "https://api.thegrid.ai") {
		t.Fatalf("Grid base URL should be thegrid.ai; got %q", g.OpenAI.baseURL)
	}
	want := map[string]bool{
		"text-standard": false, "text-prime": false, "text-max": false,
		"code-standard": false, "code-prime": false, "code-max": false,
		"agent-standard": false, "agent-prime": false, "agent-max": false,
	}
	for _, m := range models {
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Fatalf("missing expected instrument %q", k)
		}
	}
	if ems := g.EmbeddingModels(); len(ems) != 0 {
		t.Fatalf("Grid should not advertise embedding models; got %v", ems)
	}
}

// TestStripAuthorizationOnCrossOriginRedirect drives the redirect helper
// directly: a source server replies 307 to a destination server on a
// different host, and the destination server records whatever
// Authorization header it received.
// Without the policy in place the destination would see "Bearer secret";
// with it, the header is empty.
func TestStripAuthorizationOnCrossOriginRedirect(t *testing.T) {
	var gotAtSupplier string
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAtSupplier = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "ok")
	}))
	defer supplier.Close()

	grid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, supplier.URL+"/complete", http.StatusTemporaryRedirect)
	}))
	defer grid.Close()

	client := &http.Client{CheckRedirect: stripAuthorizationOnCrossOriginRedirect}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, grid.URL+"/start", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer grid-secret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status from supplier: %d", resp.StatusCode)
	}
	if gotAtSupplier != "" {
		t.Fatalf("cross-origin redirect leaked Authorization header to supplier: %q", gotAtSupplier)
	}
}

// TestKeepAuthorizationOnSameOriginRedirect is the regression guard:
// a same-origin redirect (e.g. to a different path on the same host)
// must keep the Authorization header.
func TestKeepAuthorizationOnSameOriginRedirect(t *testing.T) {
	var gotAtTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/endpoint":
			gotAtTarget = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, "ok")
		default:
			http.Redirect(w, r, "/endpoint", http.StatusTemporaryRedirect)
		}
	}))
	defer srv.Close()

	client := &http.Client{CheckRedirect: stripAuthorizationOnCrossOriginRedirect}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer grid-secret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
	if gotAtTarget == "" {
		t.Fatalf("same-origin redirect should preserve Authorization; got empty header")
	}
	if !strings.Contains(gotAtTarget, "grid-secret") {
		t.Fatalf("same-origin redirect token mismatch: %q", gotAtTarget)
	}
}

// TestGridAdapterInstallsCheckRedirect ensures that constructing the
// provider via NewGrid wires the redirect policy onto its HTTP client.
// We don't drive a full request — we just verify the field is populated
// so a future change can't silently lose the strip-on-cross-origin
// behaviour.
func TestGridAdapterInstallsCheckRedirect(t *testing.T) {
	g := NewGrid("k", 0)
	if g.OpenAI.client == nil || g.OpenAI.client.CheckRedirect == nil {
		t.Fatalf("NewGrid must install a CheckRedirect policy")
	}
	if g.OpenAI.client.CheckRedirect == nil {
		t.Fatalf("nil CheckRedirect")
	}
}
