// Package egress is the single chokepoint for outbound HTTP whose destination
// somebody configured.
//
// # Why this exists
//
// The security review found the same class of defect in five separate places,
// and the reason was structural: every outbound path built its own
// http.Client. Each one independently decided whether to set a timeout, whether
// to follow redirects, and whether to look at where the URL actually pointed.
// The answer to the third question was always "no". Fixing them one at a time
// guarantees the sixth path repeats it.
//
// The consequence is worse than an unauthorised read, because two of these paths
// send prompt content to a tenant-chosen URL and then STORE THE RESPONSE:
//
//   - An org admin sets an eval profile's endpoint.base_url. The worker POSTs the
//     prompt and completion there and writes the reply into eval_scores as the
//     score rationale, which the console renders.
//   - The same is true of a plugin manifest's spec.service.endpoint.
//
// Point either at http://169.254.169.254/latest/meta-data/iam/security-credentials/
// and the pod's cloud IAM credentials arrive in the console as an evaluation
// rationale. The fetch is server-side, the response comes back, and it is
// persisted. That is credential exfiltration through an evaluation feature.
//
// # What the guard enforces
//
//   - The destination IP is checked against the policy for its trust class,
//     AFTER DNS resolution, at connect time. See dialGuard.
//   - Every client has a timeout. A zero timeout means "wait forever", which is
//     how one unreachable vendor becomes a worker goroutine leak.
//   - Redirects are bounded, and each hop is re-checked because each hop dials
//     again. A public URL that 302s to the metadata service does not work.
//   - Authorization headers are dropped when a redirect crosses to another host,
//     so a vendor cannot harvest the API key by redirecting.
//
// # What it deliberately does not do
//
// It is not an allowlist of vendor hostnames. A self-hosted install points at
// whatever Langfuse or collector the customer runs, and enumerating that in
// Nexus would mean a product release every time a customer picks a new tool.
// FQDN-level egress control belongs in the customer's egress gateway or service
// mesh; docs/customer-self-hosted-security.md says so rather than implying Nexus
// covers it.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)
// Class is the trust level of whoever chose the destination. It is the only
// input that changes the IP policy, and it exists because "block private
// addresses" is correct for one of these and breaks the product for the other.
type Class int

const (
	// Operator means the destination came from an environment variable or Helm
	// value: the OTLP collector, the failover webhook, the Metabase URL.
	//
	// Private and loopback addresses are ALLOWED. In a self-hosted install the
	// collector is a sidecar on 127.0.0.1 or a ClusterIP on 10.x, and blocking
	// those would mean the feature only works when the customer sends telemetry
	// out of their own cluster — the opposite of what a self-hosted customer
	// wants. Anyone who can set the pod's environment already controls the pod,
	// so there is no privilege to escalate here.
	Operator Class = iota

	// Tenant means the destination came from an API request body or a database
	// row an org admin wrote: an eval profile's base_url, a plugin manifest's
	// endpoint, a credential's base_url, a preflight probe target.
	//
	// This is a request from inside the cluster made on behalf of someone who is
	// outside it, so the pod's network position is a privilege the caller does
	// not otherwise have. Private, loopback and link-local addresses are
	// REFUSED unless the operator has explicitly allowed specific ranges.
	Tenant
)

func (c Class) String() string {
	if c == Tenant {
		return "tenant"
	}
	return "operator"
}

// Blocked address ranges, by the reason they are blocked rather than by RFC, so
// that a reader can tell which entries are negotiable.
var (
	// alwaysBlocked is refused for every class including Operator. These
	// addresses have no legitimate destination semantics at all.
	alwaysBlocked = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),          // "this host", and 0.0.0.0 as a destination
		netip.MustParsePrefix("::/128"),             // unspecified
		netip.MustParsePrefix("224.0.0.0/4"),        // multicast
		netip.MustParsePrefix("ff00::/8"),           // multicast
		netip.MustParsePrefix("255.255.255.255/32"), // broadcast
	}

	// metadataBlocked is the cloud instance metadata service. Refused for every
	// class: nothing Nexus sends outbound belongs here, and this is the single
	// highest-value SSRF target in any cloud deployment. 169.254.0.0/16 covers
	// AWS/GCP/Azure/DigitalOcean/Oracle; fd00:ec2::/32 is AWS IMDS over IPv6.
	metadataBlocked = []netip.Prefix{
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fe80::/10"), // IPv6 link-local, same role
		netip.MustParsePrefix("fd00:ec2::/32"),
	}

	// tenantBlocked is refused only for Tenant destinations: reachable from the
	// pod, not reachable by the caller, therefore a privilege the caller is
	// borrowing.
	tenantBlocked = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),     // loopback: Nexus itself, sidecars
		netip.MustParsePrefix("::1/128"),         //
		netip.MustParsePrefix("10.0.0.0/8"),      // RFC1918: pods, services, the DB
		netip.MustParsePrefix("172.16.0.0/12"),   //
		netip.MustParsePrefix("192.168.0.0/16"),  //
		netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT, used by some CNIs
		netip.MustParsePrefix("fc00::/7"),        // IPv6 unique-local
		netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
		netip.MustParsePrefix("192.0.2.0/24"),    // documentation ranges, no route
		netip.MustParsePrefix("198.51.100.0/24"), //
		netip.MustParsePrefix("203.0.113.0/24"),  //
		netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	}
)

// Policy is the operator's configuration of the guard.
type Policy struct {
	// TenantAllowedCIDRs re-permits specific ranges for Tenant destinations.
	//
	// The escape hatch exists for a real deployment: a customer runs Langfuse or
	// an OTLP collector inside the cluster and wants org admins to point eval
	// plugins at it. Without this they would have to route in-cluster traffic
	// out through the internet and back. It is opt-in and narrow — a CIDR list,
	// not a boolean — so "allow 10.0.0.0/8" is a decision somebody typed rather
	// than a default.
	//
	// It cannot re-permit metadataBlocked or alwaysBlocked. There is no
	// legitimate reason to POST a customer's prompts to the metadata service,
	// and an operator who believes otherwise is mistaken.
	TenantAllowedCIDRs []netip.Prefix

	// MaxRedirects bounds the redirect chain. Zero uses defaultMaxRedirects.
	MaxRedirects int

	// allowLoopback permits tenant-class destinations on 127.0.0.0/8 and ::1.
	//
	// Unexported on purpose: no configuration path can set it. Loopback is the
	// one range that must stay closed regardless of what an operator asks for,
	// because it reaches Nexus's own listeners — including the console API on
	// the pod's own port, which would let a tenant-supplied eval endpoint call
	// back into the admin surface from inside the trust boundary. The only
	// setters are the helpers in testing.go, which exist because httptest binds
	// to loopback.
	allowLoopback bool

	// DefaultTimeout applies when a caller asks for a client with no timeout.
	// Zero uses defaultTimeout.
	DefaultTimeout time.Duration
}

const (
	defaultMaxRedirects = 3
	defaultTimeout      = 30 * time.Second
	dialTimeout         = 10 * time.Second
)

// ErrBlockedDestination is returned when an address fails the policy. Callers
// surface it to operators; it names the address and the reason but nothing about
// the request.
var ErrBlockedDestination = errors.New("egress: destination address is not permitted")

// ErrUnresolvable means the host did not resolve, which is NOT a policy failure
// and callers validating configuration must not treat it as one.
//
// A save-time check that rejected unresolvable hosts would make storing a
// perfectly good vendor URL depend on DNS being answerable from the pod at that
// instant. Private DNS zones, split-horizon resolvers and names that only exist
// once a customer finishes their own DNS change all resolve later but not now.
// Refusing the save in those cases produces a support ticket, and the address
// policy is not enforced by the save anyway — the dialer enforces it on every
// request. So configuration validators reject ErrBlockedDestination and let
// ErrUnresolvable through.
var ErrUnresolvable = errors.New("egress: destination host does not resolve")

// Guard builds HTTP clients that enforce a Policy. Safe for concurrent use; one
// per process is expected.
type Guard struct {
	policy Policy
	// resolver is swapped in tests so a hostname can be made to resolve to a
	// blocked address without depending on public DNS.
	resolver func(ctx context.Context, host string) ([]netip.Addr, error)
}

// New returns a Guard enforcing policy.
func New(policy Policy) *Guard {
	if policy.MaxRedirects <= 0 {
		policy.MaxRedirects = defaultMaxRedirects
	}
	if policy.DefaultTimeout <= 0 {
		policy.DefaultTimeout = defaultTimeout
	}
	return &Guard{policy: policy, resolver: defaultResolve}
}

func defaultResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// checkAddr applies the policy to one resolved address.
func (g *Guard) checkAddr(class Class, addr netip.Addr) error {
	addr = addr.Unmap() // an IPv4-mapped IPv6 address must be judged as IPv4

	for _, p := range alwaysBlocked {
		if prefixHas(p, addr) {
			return fmt.Errorf("%w: %s is a reserved address", ErrBlockedDestination, addr)
		}
	}
	for _, p := range metadataBlocked {
		if prefixHas(p, addr) {
			return fmt.Errorf("%w: %s is link-local, which is where cloud instance "+
				"metadata lives; this is never a valid destination", ErrBlockedDestination, addr)
		}
	}
	if class != Tenant {
		return nil
	}
	// Interface-local and other forms the prefix table can miss. Checked ahead of
	// the operator allowlist because loopback is not allowlistable: see
	// Policy.allowLoopback.
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: %s is link-local and a tenant-supplied destination "+
			"may not reach it", ErrBlockedDestination, addr)
	}
	if addr.IsLoopback() {
		if !g.policy.allowLoopback {
			return fmt.Errorf("%w: %s is loopback. A destination configured through "+
				"the API may not reach the pod's own listeners", ErrBlockedDestination, addr)
		}
		// Return here rather than fall through: 127.0.0.0/8 is also in
		// tenantBlocked, so continuing would re-reject what was just permitted.
		return nil
	}
	for _, allowed := range g.policy.TenantAllowedCIDRs {
		if prefixHas(allowed, addr) {
			return nil
		}
	}
	for _, p := range tenantBlocked {
		if prefixHas(p, addr) {
			return fmt.Errorf("%w: %s is a private address. A destination configured "+
				"through the API may only reach the public internet. If this host is "+
				"inside the cluster on purpose, the operator must add its range to "+
				"NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", ErrBlockedDestination, addr)
		}
	}
	return nil
}

// prefixHas is Prefix.Contains with the address families reconciled, because
// Contains returns false rather than an error on a family mismatch and that
// would silently pass every check.
func prefixHas(p netip.Prefix, addr netip.Addr) bool {
	if p.Addr().Is4() != addr.Is4() {
		return false
	}
	return p.Contains(addr)
}

// CheckURL validates a destination before it is stored.
//
// This runs at configuration time so an operator or org admin gets an immediate,
// explainable rejection in the console instead of a plugin that silently never
// reports a score. It is NOT the security boundary: DNS can change between the
// check and the request, so the dialer re-checks on every connection. Both are
// needed — this one for the error message, the dialer for the guarantee.
func (g *Guard) CheckURL(ctx context.Context, rawURL string, class Class) error {
	u, err := parseDestination(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()

	// A literal IP needs no resolution, and must not be handed to the resolver.
	if addr, err := netip.ParseAddr(host); err == nil {
		return g.checkAddr(class, addr)
	}
	addrs, err := g.resolver(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: %s (%v)", ErrUnresolvable, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %s resolved to no addresses", ErrUnresolvable, host)
	}
	// Every answer must pass. A round-robin record with one private answer would
	// otherwise be a coin flip.
	for _, a := range addrs {
		if err := g.checkAddr(class, a); err != nil {
			return err
		}
	}
	return nil
}

// parseDestination applies the URL-shape rules shared by every caller.
func parseDestination(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("egress: %q is not a valid URL: %w", rawURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "":
		return nil, fmt.Errorf("egress: %q has no scheme; use http:// or https://", rawURL)
	default:
		// file://, gopher://, ftp:// and friends. Go's http.Client refuses these
		// anyway, but rejecting here produces a message that says why.
		return nil, fmt.Errorf("egress: scheme %q is not permitted; use http or https", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("egress: %q has no host", rawURL)
	}
	if u.User != nil {
		// A credential in the URL would be written to logs by every layer that
		// prints a destination, and it is never how these vendors authenticate.
		// The message names the host only; echoing rawURL here would put the
		// credential into the log line that reports the problem.
		return nil, fmt.Errorf("egress: the URL for host %q embeds credentials; "+
			"put the key in the configured secret instead", u.Hostname())
	}
	return u, nil
}

// Dialer returns a *net.Dialer that enforces the policy for class, so a
// non-HTTP caller (SMTP, raw TCP, gRPC) gets the same connect-time address
// check an http.Client gets. Connect itself caps at dialTimeout (10 s);
// callers needing a longer overall send budget wrap the resulting net.Conn
// in SetDeadline rather than widening the connect timeout, so the policy
// path remains unconditionally bounded.
//
// The IP policy runs at connect time against the literal address the
// socket is about to dial, so a hostname that resolves to a public address
// when validated and a private one when fetched still gets refused.
func (g *Guard) Dialer(class Class) *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
		// Re-stated from Client so the rationale survives a copy: the
		// control hook fires at connect time, not at config check time,
		// so a rebinding DNS cannot smuggle a private address into a
		// permit-against-public validation.
		Control: func(_, address string, _ syscall.RawConn) error {
			return g.checkDialAddress(class, address)
		},
	}
}
//
// timeout is the whole-request budget. A non-positive value gets
// Policy.DefaultTimeout rather than Go's zero-means-forever, because an
// unbounded outbound request is how a single unreachable vendor turns into a
// goroutine leak that outlives the trace it was evaluating.
func (g *Guard) Client(class Class, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = g.policy.DefaultTimeout
	}
	dialer := g.Dialer(class)
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			ExpectContinueTimeout: 1 * time.Second,
			MaxIdleConns:          32,
			// Per-host reuse matters: the eval paths call one vendor repeatedly,
			// and without this Go's default of 2 makes every third concurrent
			// evaluation pay a fresh TLS handshake.
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			// Proxies are deliberately not honoured. HTTP_PROXY in the pod
			// environment would route around the dialer check, since the socket
			// would connect to the proxy's address and the real destination
			// would travel in the request line.
			Proxy: nil,
		},
		CheckRedirect: g.checkRedirect,
	}
}

// checkDialAddress validates the "ip:port" a dial is about to use.
func (g *Guard) checkDialAddress(class Class, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot parse dial address %q", ErrBlockedDestination, address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Control is documented to receive a literal address. If it does not,
		// fail closed rather than assume it is fine.
		return fmt.Errorf("%w: dial address %q is not a literal IP", ErrBlockedDestination, host)
	}
	return g.checkAddr(class, addr)
}

// checkRedirect bounds the chain and strips credentials across hosts.
//
// The IP policy needs no work here: each hop opens a new connection through the
// same guarded dialer, so a 302 to the metadata service is refused by Control.
// What this adds is a bound on the chain, and dropping the Authorization header
// when the host changes so a vendor cannot collect the API key by answering 302
// with a Location pointing at itself.
func (g *Guard) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= g.policy.MaxRedirects {
		return fmt.Errorf("egress: stopped after %d redirects", len(via))
	}
	prev := via[len(via)-1]
	if !sameHost(prev.URL.Host, req.URL.Host) {
		req.Header.Del("Authorization")
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Cookie")
		// Vendor-specific key headers. Missing one is a leak, so the list errs
		// toward dropping too much: a cross-host redirect that needed the header
		// is not a flow Nexus supports.
		for _, h := range []string{
			"X-Api-Key", "Api-Key", "X-Goog-Api-Key", "X-Datadog-Api-Key",
			"X-Dd-Api-Key", "Dd-Api-Key", "X-Arize-Api-Key", "X-Confident-Api-Key",
			"Anthropic-Version", "X-Langsmith-Api-Key", "X-Nexus-Signature",
		} {
			req.Header.Del(h)
		}
	}
	// A downgrade to cleartext after the operator configured https means the
	// request would leave the cluster unencrypted with the body intact.
	if strings.EqualFold(prev.URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http") {
		return fmt.Errorf("egress: refusing redirect from https to http (%s)", req.URL.Host)
	}
	return nil
}

func sameHost(a, b string) bool {
	return strings.EqualFold(stripPort(a), stripPort(b))
}

func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
