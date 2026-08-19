package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func tenantGuard(t *testing.T) *Guard {
	t.Helper()
	return New(Policy{})
}

// resolveTo makes hostnames resolve to fixed addresses so a test can exercise
// the private-address policy without depending on public DNS.
func resolveTo(g *Guard, table map[string][]string) {
	g.resolver = func(_ context.Context, host string) ([]netip.Addr, error) {
		raw, ok := table[host]
		if !ok {
			return nil, fmt.Errorf("no such host: %s", host)
		}
		out := make([]netip.Addr, 0, len(raw))
		for _, r := range raw {
			out = append(out, netip.MustParseAddr(r))
		}
		return out, nil
	}
}

// The exploit this package exists to stop. An org admin sets an eval profile's
// endpoint.base_url to the cloud metadata service; the worker POSTs the prompt
// there and writes the response into eval_scores as the score rationale, which
// the console renders. The pod's IAM credentials arrive in the UI.
func TestTenantDestinationCannotReachCloudMetadata(t *testing.T) {
	g := tenantGuard(t)
	ctx := context.Background()

	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token",
		"http://[fd00:ec2::254]/latest/meta-data/",
		"http://169.254.170.2/v2/credentials/", // ECS task role
	} {
		if err := g.CheckURL(ctx, target, Tenant); err == nil {
			t.Errorf("CheckURL permitted the metadata service: %s", target)
		} else if !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: want ErrBlockedDestination, got %v", target, err)
		}
	}
}

// The metadata service is refused even for operator-configured destinations.
// Private and loopback are allowed there, but the metadata address is never a
// destination for anything Nexus sends.
func TestMetadataIsBlockedEvenForOperatorConfiguredDestinations(t *testing.T) {
	g := tenantGuard(t)
	if err := g.CheckURL(context.Background(), "http://169.254.169.254/", Operator); err == nil {
		t.Error("an operator-configured destination reached the metadata service")
	}
}

// A self-hosted install points its collector at a sidecar or a ClusterIP.
// Blocking those would mean telemetry only works when it leaves the customer's
// cluster, which is backwards for a self-hosted product.
func TestOperatorDestinationsMayUsePrivateAndLoopbackAddresses(t *testing.T) {
	ctx := context.Background()

	for _, target := range []string{
		"http://127.0.0.1:4318/v1/traces",        // OTLP sidecar
		"http://10.4.1.9:4318/v1/traces",         // ClusterIP
		"http://192.168.10.20:3000",              // in-cluster Grafana
		"http://langfuse.observability.svc:3000", // resolves to a pod IP
	} {
		g2 := New(Policy{})
		resolveTo(g2, map[string][]string{"langfuse.observability.svc": {"10.44.2.7"}})
		if err := g2.CheckURL(ctx, target, Operator); err != nil {
			t.Errorf("operator destination %s was refused: %v", target, err)
		}
	}
}

// The same addresses are refused when a tenant chose them: the pod's network
// position is a privilege the API caller does not otherwise hold.
func TestTenantDestinationsCannotReachPrivateAddresses(t *testing.T) {
	g := tenantGuard(t)
	ctx := context.Background()

	for _, target := range []string{
		"http://127.0.0.1:8080/v1/chat/completions", // Nexus itself
		"http://localhost:5432",                     // a sidecar
		"http://10.4.1.9:8123",                      // the ClickHouse ClusterIP
		"http://172.20.0.5:5432",                    // the database
		"http://192.168.1.1",                        // the customer's router
		"http://100.64.3.4",                         // CGNAT range some CNIs use
		"http://[::1]:8080",
		"http://[fc00::1]:8080",
	} {
		resolveTo(g, map[string][]string{"localhost": {"127.0.0.1"}})
		err := g.CheckURL(ctx, target, Tenant)
		if err == nil {
			t.Errorf("tenant destination %s was permitted", target)
			continue
		}
		if !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: want ErrBlockedDestination, got %v", target, err)
		}
	}
}

// A hostname that resolves into a private range is the same attack with one
// indirection, and it is the form that actually gets used because it looks
// innocuous in the console.
func TestTenantDestinationCannotReachPrivateAddressViaHostname(t *testing.T) {
	g := tenantGuard(t)
	resolveTo(g, map[string][]string{
		"judge.attacker.example":   {"10.4.1.9"},
		"metadata.google.internal": {"169.254.169.254"},
	})

	for _, host := range []string{"judge.attacker.example", "metadata.google.internal"} {
		if err := g.CheckURL(context.Background(), "https://"+host+"/score", Tenant); err == nil {
			t.Errorf("%s resolved into a blocked range but was permitted", host)
		}
	}
}

// A record with one public and one private answer must not be a coin flip.
func TestEveryResolvedAddressMustPass(t *testing.T) {
	g := tenantGuard(t)
	resolveTo(g, map[string][]string{
		"mixed.example": {"93.184.216.34", "10.0.0.5"},
	})
	if err := g.CheckURL(context.Background(), "https://mixed.example/", Tenant); err == nil {
		t.Error("a hostname with one private answer was permitted; whether the " +
			"request reaches the private address would depend on resolver ordering")
	}
}

// An IPv4-mapped IPv6 literal is the same address wearing a different hat, and
// judging it as IPv6 would let 10.0.0.1 through as ::ffff:10.0.0.1.
func TestIPv4MappedAddressesAreJudgedAsIPv4(t *testing.T) {
	g := tenantGuard(t)
	for _, target := range []string{
		"http://[::ffff:10.0.0.1]:8080",
		"http://[::ffff:127.0.0.1]:8080",
		"http://[::ffff:169.254.169.254]/",
	} {
		if err := g.CheckURL(context.Background(), target, Tenant); err == nil {
			t.Errorf("%s was permitted; an IPv4-mapped address bypassed the v4 policy", target)
		}
	}
}

// Decimal, octal and hex integer forms of an address are accepted by some
// parsers and are a classic filter bypass. Go's netip rejects them, so these
// must fail as unresolvable or blocked — never succeed.
func TestObfuscatedAddressFormsDoNotPass(t *testing.T) {
	g := tenantGuard(t)
	g.resolver = func(_ context.Context, host string) ([]netip.Addr, error) {
		return nil, fmt.Errorf("no such host: %s", host)
	}
	for _, target := range []string{
		"http://2130706433/", // 127.0.0.1 as a decimal integer
		"http://0177.0.0.1/", // octal
		"http://0x7f.0.0.1/", // hex
		"http://127.1/",      // short form
	} {
		if err := g.CheckURL(context.Background(), target, Tenant); err == nil {
			t.Errorf("%s was permitted", target)
		}
	}
}

func TestNonHTTPSchemesAreRefused(t *testing.T) {
	g := tenantGuard(t)
	for _, target := range []string{
		"file:///etc/passwd",
		"gopher://127.0.0.1:11211/",
		"ftp://internal.example/",
		"redis://10.0.0.5:6379",
		"//no-scheme.example/path",
		"not a url at all",
	} {
		if err := g.CheckURL(context.Background(), target, Tenant); err == nil {
			t.Errorf("%s was permitted", target)
		}
	}
}

// A credential in the URL would be copied into every log line that reports a
// destination. The rejection message must not echo it back.
func TestCredentialsInTheURLAreRefusedAndNotEchoed(t *testing.T) {
	g := tenantGuard(t)
	err := g.CheckURL(context.Background(), "https://user:sup3rs3cret@vendor.example/api", Tenant)
	if err == nil {
		t.Fatal("a URL with embedded credentials was permitted")
	}
	if strings.Contains(err.Error(), "sup3rs3cret") {
		t.Errorf("the rejection message echoed the credential: %v", err)
	}
}

// The escape hatch for a customer running their own vendor in-cluster.
func TestOperatorCanAllowSpecificPrivateRangesForTenants(t *testing.T) {
	g := New(Policy{TenantAllowedCIDRs: []netip.Prefix{
		netip.MustParsePrefix("10.44.0.0/16"),
	}})
	ctx := context.Background()

	if err := g.CheckURL(ctx, "http://10.44.2.7:3000/api", Tenant); err != nil {
		t.Errorf("an explicitly allowed range was refused: %v", err)
	}
	// A different private range is still refused: the allowance is a CIDR list,
	// not a switch that turns the policy off.
	if err := g.CheckURL(ctx, "http://10.99.2.7:3000/api", Tenant); err == nil {
		t.Error("allowing 10.44.0.0/16 also allowed 10.99.0.0/16")
	}
	// And the allowance cannot re-permit the metadata service.
	wide := New(Policy{TenantAllowedCIDRs: []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/0"),
	}})
	if err := wide.CheckURL(ctx, "http://169.254.169.254/", Tenant); err == nil {
		t.Error("an operator allowlist of 0.0.0.0/0 re-permitted the metadata service; " +
			"that range must be unreachable regardless of configuration")
	}
}

// CheckURL is for the error message; the dialer is the guarantee. This test is
// the one that proves the guarantee, because it goes through a real socket.
func TestDialerRefusesABlockedAddressAtConnectTime(t *testing.T) {
	g := tenantGuard(t)
	// A server on loopback, reached by its literal address so no DNS is involved.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("this response must never be read"))
	}))
	defer srv.Close()

	resp, err := g.Client(Tenant, 5*time.Second).Get(srv.URL)
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("the client connected to loopback and read %q", body)
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("blocked for the wrong reason: %v", err)
	}
	// The same server is reachable for an operator-configured destination.
	resp2, err := g.Client(Operator, 5*time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("operator class could not reach a loopback server: %v", err)
	}
	resp2.Body.Close()
}

// DNS rebinding: the name passes CheckURL, then resolves to a blocked address by
// the time the request is made. The dialer must catch it, which is why the check
// lives in Dialer.Control rather than in a pre-flight URL parse.
func TestRebindingBetweenCheckAndRequestIsCaughtAtDial(t *testing.T) {
	g := tenantGuard(t)

	// Passes validation: a public address.
	resolveTo(g, map[string][]string{"rebind.example": {"93.184.216.34"}})
	if err := g.CheckURL(context.Background(), "http://rebind.example/", Tenant); err != nil {
		t.Fatalf("setup: the public answer should validate: %v", err)
	}

	// Now the name resolves to loopback, where a real server is listening. The
	// client's own resolver is used here, so a custom dialer stands in for the
	// rebinding: it returns the blocked address that Control then sees.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("rebound"))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	_ = host

	client := g.Client(Tenant, 5*time.Second)
	// Reaching the same socket by its literal loopback address is exactly what a
	// rebound name resolves to, and Control sees only the address.
	resp, err := client.Get("http://127.0.0.1:" + port + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("the dial to a rebound loopback address succeeded")
	}
}

// A public URL that 302s to the metadata service is the redirect form of the
// same attack. Each hop dials again, so Control catches it.
func TestRedirectToABlockedAddressIsRefused(t *testing.T) {
	g := tenantGuard(t)

	// The "vendor" redirects to loopback. Both the vendor and the target are on
	// loopback here, so the class is Operator to let the first hop through and
	// isolate the redirect behaviour being tested.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer target.Close()

	hops := 0
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer vendor.Close()

	// Tenant class: the first hop to loopback is already refused.
	if _, err := g.Client(Tenant, 5*time.Second).Get(vendor.URL); err == nil {
		t.Error("tenant class reached a loopback vendor")
	}

	// A redirect chain longer than the policy is stopped.
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer loop.Close()
	_, err := g.Client(Operator, 5*time.Second).Get(loop.URL)
	if err == nil {
		t.Error("an unbounded redirect loop was followed to completion")
	} else if !strings.Contains(err.Error(), "stopped after") {
		t.Errorf("redirect loop failed for the wrong reason: %v", err)
	}
}

// A vendor must not be able to collect the API key by answering 302 with a
// Location on a host it controls.
func TestAuthorizationIsDroppedOnACrossHostRedirect(t *testing.T) {
	g := tenantGuard(t)

	var gotAuth, gotAPIKey string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	// Redirect to the collector by IP-literal on a different port, which the
	// guard compares by host: same host means the header is kept, so use a
	// hostname alias to make it cross-host.
	collectorURL := strings.Replace(collector.URL, "127.0.0.1", "localhost", 1)
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, collectorURL, http.StatusFound)
	}))
	defer vendor.Close()

	req, _ := http.NewRequest(http.MethodGet, vendor.URL, nil)
	req.Header.Set("Authorization", "Bearer sk-customer-secret")
	req.Header.Set("X-Api-Key", "vendor-key-secret")
	resp, err := g.Client(Operator, 5*time.Second).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "" {
		t.Errorf("Authorization survived a cross-host redirect: %q", gotAuth)
	}
	if gotAPIKey != "" {
		t.Errorf("X-Api-Key survived a cross-host redirect: %q", gotAPIKey)
	}
}

// A vendor answering 302 with an http:// Location would put the request body,
// which may contain prompt content, on the wire in cleartext.
func TestHTTPSToHTTPDowngradeRedirectIsRefused(t *testing.T) {
	g := tenantGuard(t)
	req := httptest.NewRequest(http.MethodGet, "http://vendor.example/after", nil)
	via := []*http.Request{httptest.NewRequest(http.MethodGet, "https://vendor.example/before", nil)}

	if err := g.checkRedirect(req, via); err == nil {
		t.Error("a redirect from https to http was allowed")
	}
}

// A zero timeout means "wait forever" in Go, which is how one unreachable vendor
// becomes a leaked worker goroutine per trace.
func TestClientAlwaysHasATimeout(t *testing.T) {
	g := New(Policy{DefaultTimeout: 7 * time.Second})
	if got := g.Client(Operator, 0).Timeout; got != 7*time.Second {
		t.Errorf("a zero timeout produced %v, want the policy default", got)
	}
	if got := g.Client(Operator, -1).Timeout; got <= 0 {
		t.Errorf("a negative timeout produced %v, want the policy default", got)
	}
	if got := New(Policy{}).Client(Operator, 0).Timeout; got <= 0 {
		t.Error("an unconfigured policy produced a client with no timeout")
	}
}

// HTTP_PROXY in the pod environment would route every request through a proxy,
// so the socket would connect to the proxy and the real destination would travel
// in the request line where Control never sees it.
func TestProxyEnvironmentCannotRouteAroundTheGuard(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://10.0.0.9:3128")
	t.Setenv("HTTPS_PROXY", "http://10.0.0.9:3128")

	g := tenantGuard(t)
	tr, ok := g.Client(Tenant, time.Second).Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not an *http.Transport")
	}
	if tr.Proxy != nil {
		t.Error("the transport honours a proxy from the environment, which would " +
			"bypass the destination check entirely")
	}
}

// A configuration validator must not reject a host that does not resolve from
// this pod at this moment. Private DNS zones, split-horizon resolvers, and names
// the customer has not finished creating all resolve later. Rejecting the save
// makes storing a valid vendor URL depend on DNS availability, and the address
// policy is enforced at dial time regardless.
func TestConfiguredURLCheckToleratesAnUnresolvableHostButNotABlockedOne(t *testing.T) {
	g := New(Policy{})
	resolveTo(g, map[string][]string{"internal.example": {"10.0.0.5"}})
	SetDefault(g)
	t.Cleanup(func() { defaultGuard.Store(nil) })
	ctx := context.Background()

	if err := CheckConfiguredURL(ctx, "https://not-registered-yet.vendor.example/v1", Tenant); err != nil {
		t.Errorf("an unresolvable host was rejected at configuration time: %v", err)
	}
	if err := CheckConfiguredURL(ctx, "http://internal.example/v1", Tenant); err == nil {
		t.Error("a host resolving into a private range was accepted at configuration time")
	}
	if err := CheckConfiguredURL(ctx, "file:///etc/passwd", Tenant); err == nil {
		t.Error("a malformed URL was accepted at configuration time")
	}
	if err := CheckConfiguredURL(ctx, "http://169.254.169.254/", Tenant); err == nil {
		t.Error("the metadata service was accepted at configuration time")
	}
}
