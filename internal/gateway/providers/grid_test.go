package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// 9 task-tier instruments (3 tiers × 3 standards) plus 8 lab-latest
	// model-family markets. Membership of the lab-latest half is asserted
	// in grid_catalog_test.go.
	if len(models) != 17 {
		t.Fatalf("Grid should expose 17 instruments (9 task tiers + 8 lab-latest); got %d (%v)", len(models), models)
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
// authorization, cookie, or proxy-auth header it received. Without the
// policy in place the destination would see "Bearer grid-secret" along
// with the cookie and proxy-auth; with it, every credential-shaped
// header must be empty and the Host header must be cleared so the
// second hop binds to its own hostname.
func TestStripAuthorizationOnCrossOriginRedirect(t *testing.T) {
	var (
		gotAuth        string
		gotXAPI        string
		gotCookie      string
		gotProxyAuth   string
		gotHost        string
		gotRemoteAddr  string
	)
	supplier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPI = r.Header.Get("x-api-key")
		gotCookie = r.Header.Get("Cookie")
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		gotHost = r.Host
		gotRemoteAddr = r.RemoteAddr
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
	req.Header.Set("x-api-key", "anthropic-secret")
	req.Header.Set("Cookie", "session=leak")
	req.Header.Set("Proxy-Authorization", "Basic leak")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status from supplier: %d", resp.StatusCode)
	}

	if gotAuth != "" {
		t.Fatalf("cross-origin redirect leaked Authorization header to supplier: %q", gotAuth)
	}
	if gotXAPI != "" {
		t.Fatalf("cross-origin redirect leaked x-api-key header to supplier: %q", gotXAPI)
	}
	if gotCookie != "" {
		t.Fatalf("cross-origin redirect leaked Cookie header to supplier: %q", gotCookie)
	}
	if gotProxyAuth != "" {
		t.Fatalf("cross-origin redirect leaked Proxy-Authorization header to supplier: %q", gotProxyAuth)
	}
	if gotHost != strings.TrimPrefix(supplier.URL, "http://") {
		t.Fatalf("cross-origin redirect left stale Host header on supplier: %q (want %q)", gotHost, supplier.URL)
	}
	// Sanity: the request actually reached the supplier, not a wholly
	// different box. If RemoteAddr is empty (test harness quirk), do not
	// fail the test, but never let it be the grid host.
	if gotRemoteAddr != "" && strings.Contains(gotRemoteAddr, strings.TrimPrefix(grid.URL, "http://")) {
		t.Fatalf("request appears to have stayed on the source host: %q", gotRemoteAddr)
	}
}

// TestRedirectHopCapStopsLongChains covers the loop termination rule:
// if a redirect Location is influenced (or mis-built) to point back at
// the same source, the policy must bail out after maxUpstreamRedirects
// hops instead of looping forever or exhausting the connection pool.
//
// We exercise the policy directly because httptest redirect loops are
// notoriously fragile; the cap is enforced inside the policy before any
// network access happens, so a unit-level call is enough.
func TestRedirectHopCapStopsLongChains(t *testing.T) {
	src := mustURL("https://hopper.test/start")
	chain := make([]*http.Request, 0, maxUpstreamRedirects+1)
	for i := 0; i < maxUpstreamRedirects; i++ {
		r, _ := http.NewRequest(http.MethodGet, src.String(), nil)
		r.URL = src
		chain = append(chain, r)
	}
	candidate, _ := http.NewRequest(http.MethodGet, src.String(), nil)
	candidate.URL = src

	err := stripAuthorizationOnCrossOriginRedirect(candidate, chain)
	if !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("expected ErrUseLastResponse past %d hops, got %T %v", maxUpstreamRedirects, err, err)
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

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
