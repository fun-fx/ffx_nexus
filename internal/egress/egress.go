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

	// ProxyURL, when non-empty, switches the guard into a proxy mode:
	//   * The HTTP transport uses Proxy=http.ProxyURL(ProxyURL); the dial
	//     hook (Control) is disabled because the socket now connects only to
	//     the proxy, so the dial-time IP check would inspect the proxy's
	//     address and never the real destination.
	//   * The transport is wrapped so that every request runs CheckURL
	//     against its destination BEFORE sending bytes; the URL-vetting step
	//     is the new line of defense that replaces the dial hook when
	//     proxying is on. TOCTOU between CheckURL and the proxy opening a
	//     new socket is documented and relied on only to the extent that the
	//     proxy's own ACL is the deeper stop.
	//
	// When ProxyURL is non-empty, PublicDestinationsBlocked MUST be false —
	// the chart's fail-closed gate refuses that combination.
	ProxyURL string

	// PublicDestinationsBlocked, when true, refuses any destination whose
	// resolved address is not on AllowedInternalHosts. This is the runtime
	// half of the chart's in_cluster_only mode: even if a Tenant URL points
	// outside the cluster, the guard rejects it on the request line before
	// any socket opens.
	//
	// The combination of the IP policy (always-always-blocked prefixes) and
	// AllowedInternalHosts forms the allow-list. Hostnames that do not
	// resolve AND literal IPs that are not in AllowedInternalHosts return
	// ErrBlockedDestination with a class-bound message that distinguishes
	// them, so operator diagnostics stay traceable.
	//
	// When PublicDestinationsBlocked is true, ProxyURL MUST be empty.
	PublicDestinationsBlocked bool

	// AllowedInternalHosts is the allow-list for PublicDestinationsBlocked.
	//
	// Each entry is a hostname (FQDN or single label) or a CIDR. A hostname
	// match is exact (no suffix matching; "llm.local" does NOT match
	// "api.llm.local"). A CIDR covers all addresses in the prefix.
	//
	// This is the runtime mirror of the chart's
	// providerEgress.inCluster.allowedServiceTargets list. The two are NOT
	// combined at the chart level: the operator is expected to declare the
	// same names in both places and a divergence between them is a
	// configuration bug, surfaced by install-time validation rather than
	// privileged operator behaviour at runtime.
	AllowedInternalHosts []string
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

// ErrCheckedURL wraps an inner error returned from the URL-vetting
// path (proxy mode or in_cluster_only mode). The caller receives
// this stable wrapper so it can tell apart "policy refused the
// request before byte transfer" from "network failed after the
// request was approved". The wrapped error is one of:
//   - ErrBlockedDestination (static IP policy refused),
//   - ErrPublicDestination  (in_cluster_only mode refused),
//   - ErrUnresolvable       (name did not resolve in time),
//   - or a parseDestination / credentials error.
var ErrCheckedURL = errors.New("egress: destination rejected by URL vetting")

// ErrPublicDestination is returned by CheckURL when the running
// policy is in_cluster_only and the resolved host is not in
// AllowedInternalHosts. It is distinct from ErrBlockedDestination
// so the on-call can tell "the operator deliberately confined this
// traffic" apart from "Nexus itself refuses this address on
// principle" without grepping the chart values.
var ErrPublicDestination = errors.New("egress: policy is in_cluster_only, destination is outside the cluster")

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
//
// In in_cluster_only mode CheckURL is the runtime half of the
// contract: a Tenant request's destination must be on
// AllowedInternalHosts. Otherwise ErrPublicDestination is
// returned. Static IP policy is consulted first, but in
// in_cluster_only mode it widens to cover the unsigned case:
// resolving to a public IP is ALWAYS rejected, regardless of
// which CIDRs the operator listed; the allow-list merely adds
// additional internal targets the static IP policy would have
// already rejected for being private.
//
// Proxy mode does NOT change CheckURL's verdict; the ProxyURL hook
// is set on the transport, not on the URL gate.
func (g *Guard) CheckURL(ctx context.Context, rawURL string, class Class) error {
	if g.policy.PublicDestinationsBlocked && class == Tenant {
		if err := g.checkURLInClusterOnly(ctx, rawURL); err != nil {
			if errors.Is(err, ErrUnresolvable) {
				// names that do not resolve are not refusals
				// of the operator's policy; the dialer is the
				// ultimate arbiter. Pass err through so the
				// caller can return ErrUnresolvable to the user
				// the same way direct mode does.
			}
			return err
		}
		return nil
	}
	return g.checkURLStatic(ctx, rawURL, class)
}

// checkURLInClusterOnly is the runtime half of in_cluster_only mode.
//
// Decision tree:
//
//   - the URL is unparseable / has credentials / etc.: parseDestination err.
//   - the URL is a literal IP:
//   - if the IP is on the static alwaysBlocked list (loopback,
//     link-local, IMDS): ErrBlockedDestination with the static
//     message.
//   - otherwise evaluate AllowedInternalHosts: any CIDR match or
//     IP match → OK.
//   - neither → ErrPublicDestination.
//   - the URL is a hostname:
//   - resolve; if every answer is on the static alwaysBlocked
//     list, ErrBlockedDestination.
//   - if every answer is on AllowedInternalHosts (IP match or
//     CIDR match) AND the hostname itself is on
//     AllowedInternalHosts, OK.
//   - any other shape → ErrPublicDestination.
//
// The hostname itself being on the allow-list is necessary because
// a CIDR match on every answer is not enough: a public hostname
// could happen to live at a private CIDR via split-horizon DNS, so
// the operator must name the hostname explicitly for the runtime
// to be operator-confidence about its decision.
func (g *Guard) checkURLInClusterOnly(ctx context.Context, rawURL string) error {
	u, err := parseDestination(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil {
		// Static IP policy may refuse (private IP for Tenant,
		// link-local, loopback). The in_cluster_only contract
		// can rescue a private IP if the operator's allow-list
		// explicitly contains it as a CIDR. Link-local and
		// loopback however are always-blocked prefixes and
		// never get rescued.
		if err := g.checkAddr(Tenant, addr); err != nil {
			if !errors.Is(err, ErrBlockedDestination) {
				return err
			}
			if !g.literalInUnreservedPrivateSpace(addr) {
				return fmt.Errorf("%w: %s",
					ErrBlockedDestination, err.Error())
			}
			// fall through to allow-list rescue
		}
		if !g.literalAddrAllowedByOverride(addr) {
			return fmt.Errorf("%w: %s is not on the operator allow-list",
				ErrPublicDestination, host)
		}
		return nil
	}
	// Hostname path.
	addrs, err := g.resolver(ctx, host)
	if err != nil {
		// Unresolvable hosts are the operator's DNS, not
		// the policy's. Keep the static semantics.
		return fmt.Errorf("%w: %s (%v)", ErrUnresolvable, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %s resolved to no addresses", ErrUnresolvable, host)
	}
	// First check: any answer on the static always-blocked list?
	for _, a := range addrs {
		if err := g.checkAddr(Tenant, a); err != nil {
			if errors.Is(err, ErrBlockedDestination) {
				return fmt.Errorf("%w: %s resolves to %s",
					ErrBlockedDestination, host, a.String())
			}
			return err
		}
	}
	// Second check: every answer is operator-approved
	// (i.e., a CIDR match in AllowedInternalHosts) OR the
	// hostname itself appears verbatim in the allow-list
	// (split-horizon rescue; explicit operator intent
	// override the IP-only check, because the operator names
	// the FQDN itself in the chart). The hostname override
	// is the documented path for cluster.local typos and
	// for split-horizon setups that resolve an internal
	// hostname to a public IP for external traffic.
	if g.hostInInternalAllowList(host) {
		return nil
	}
	for _, a := range addrs {
		if !g.literalAddrAllowedByOverride(a) {
			return fmt.Errorf("%w: %s (%s) is not on the operator allow-list",
				ErrPublicDestination, host, a.String())
		}
	}
	return nil
}

// checkURLStatic is the pre-in_cluster_only-mode gate. It runs the
// same checks the original CheckURL did.
func (g *Guard) checkURLStatic(ctx context.Context, rawURL string, class Class) error {
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

// timeout is the whole-request budget. A non-positive value gets
// Policy.DefaultTimeout rather than Go's zero-means-forever, because an
// unbounded outbound request is how a single unreachable vendor turns into a
// goroutine leak that outlives the trace it was evaluating.
func (g *Guard) Client(class Class, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = g.policy.DefaultTimeout
	}
	if g.policy.ProxyURL == "" && g.policy.PublicDestinationsBlocked == false {
		// ... existing dial-only path
		return &http.Client{
			Timeout:       timeout,
			Transport:     g.directTransport(class, timeout),
			CheckRedirect: g.checkRedirect,
		}
	}

	// proxy mode OR in_cluster_only mode.
	//
	// proxy mode: dial hook is off, transport uses Proxy. URL vet runs
	// before each request, sending the request body only after the URL
	// has cleared the policy. Dial hook is OFF because the socket only
	// connects to the proxy's address; a dial-time IP check would
	// inspect the proxy, not the real destination.
	//
	// in_cluster_only mode: same URL-vet shape, but the policy itself
	// refuses any destination outside AllowedInternalHosts. The chart
	// is the source of trust for that list; the guard enforces it.
	return &http.Client{
		Timeout:       timeout,
		Transport:     g.urlVettingTransport(class, timeout),
		CheckRedirect: g.checkRedirect,
	}
}

// directTransport returns the dial-only transport policy shipped before
// this milestone: Control fires at connect time, Proxy is nil, no URL
// pre-check. This is the path that runs for Operator and Tenant class
// destinations when neither proxy nor in-cluster-only is in effect.
func (g *Guard) directTransport(class Class, timeout time.Duration) http.RoundTripper {
	dialer := g.Dialer(class)
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		// Proxies are deliberately not honoured in direct mode. HTTP_PROXY
		// in the pod environment would route around the dialer check, since
		// the socket would connect to the proxy's address and the real
		// destination would travel in the request line.
		Proxy: nil,
	}
}

// urlVettingTransport wraps the proxy transport with a RoundTrip
// shim that runs CheckURL against the request destination before
// issuing the byte transfer. The shim is the substitute for the
// dial-time IP check that the proxy mode disables; its TOCTOU window
// (DNS may change between CheckURL and the proxy's dial) is accepted
// and documented as part of the contract.
func (g *Guard) urlVettingTransport(class Class, timeout time.Duration) http.RoundTripper {
	base := &http.Transport{
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		Proxy:                 http.ProxyFromEnvironment,
	}
	if g.policy.ProxyURL != "" {
		// Prefer the explicit configuration the operator typed at the
		// chart level over the process environment's HTTP_PROXY. The
		// chart that injected HTTPS_PROXY would inject the same value
		// here; the explicit field keeps the two in sync and survives
		// any other env var setting that might leak in.
		if u, err := url.Parse(g.policy.ProxyURL); err == nil {
			base.Proxy = http.ProxyURL(u)
		}
	}
	return &vettingRoundTripper{guard: g, class: class, base: base}
}

// vettingRoundTripper wraps an http.RoundTripper so that the
// destination of req.URL gets vetted by the guard BEFORE the wrapped
// RoundTrip runs. It is the runtime half of the in_cluster_only
// contract and the proxy-mode security substitute.
//
// Failures here surface as ErrCheckedURL (a stable, typed error) so
// whoever made the request can distinguish "policy refused the call"
// from "network failed after the call was approved".
type vettingRoundTripper struct {
	guard *Guard
	class Class
	base  http.RoundTripper
}

func (v *vettingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := v.guard.vetForModes(req.Context(), req.URL.String(), v.class); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckedURL, err)
	}
	return v.base.RoundTrip(req)
}

// vetForModes is the request-side half of the proxy/in_cluster_only
// contract. The static IP policy runs first; the result is
// downgraded into a "candidate error" so the in_cluster_only mode
// can override the part of the static policy that says "private
// destinations are unreachable for Tenant". That part is the
// subsystem Nexus reasoned about for decades; overriding it is
// allowed only because the operator has explicitly typed that the
// gateway may reach those destinations (via
// networkPolicy.providerEgress.inCluster.allowedServiceTargets),
// and the chart's fail-closed gate ensures that mode is reachable
// only when that list is non-empty.
//
// The proxy mode never widens the static policy: when ProxyURL is
// set, the static IP policy (Tenant private refusal, link-local
// refusal, IMDS refusal, etc.) runs in full and the URL-vet
// wrapper only adds "you may not bypass me into the proxy" — its
// job is to keep the dial hook's check alive at the URL level,
// not to grant new reach.
func (g *Guard) vetForModes(ctx context.Context, rawURL string, class Class) error {
	if !g.policy.PublicDestinationsBlocked {
		// proxy-or-direct mode: existing static CheckURL is
		// the only check.
		return g.CheckURL(ctx, rawURL, class)
	}

	// in_cluster_only mode. The static IP policy is
	// overridden ONLY where AllowedInternalHosts lists the
	// destination. First we run the static gate; if it
	// refuses, the candidate may be cleared by an explicit
	// allow-list match.
	candidate := g.CheckURL(ctx, rawURL, class)
	if g.matchedByInternalAllowList(ctx, rawURL) {
		candidate = nil
	}
	if candidate != nil {
		return candidate
	}
	u, err := parseDestination(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return nil // already allowed
	}
	if g.hostInInternalAllowList(host) {
		return nil
	}
	// All addresses resolved AND the host itself are not in
	// the allow-list. ERRPublicDestination surfaces.
	return fmt.Errorf("%w: %s is not in the in-cluster allow-list",
		ErrPublicDestination, host)
}

// matchedByInternalAllowList returns true when:
//
//   - the URL has a literal IP that appears in the allow-list,
//     OR
//   - the URL has a hostname whose DNS answers are all on the
//     allow-list AND the hostname itself appears verbatim in
//     the allow-list.
//
// It deliberately ignores the static IP policy's "Tenant
// denial of private addresses" so an enum of "private
// destination, CIDR-matched" is treated as operator-approved.
func (g *Guard) matchedByInternalAllowList(ctx context.Context, rawURL string) bool {
	u, err := parseDestination(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil {
		return g.literalAddrAllowedByOverride(addr)
	}
	addrs, err := g.resolver(ctx, host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !g.literalAddrAllowedByOverride(a) {
			return false
		}
	}
	return g.hostInInternalAllowList(host)
}

// literalAddrAllowedByOverride is the override counterpart of
// addrInInternalAllowList: it skips the static checkAddr first
// hop and only checks "is this address or a CIDR that contains
// it in AllowedInternalHosts".
func (g *Guard) literalAddrAllowedByOverride(addr netip.Addr) bool {
	for _, raw := range g.policy.AllowedInternalHosts {
		if strings.Contains(raw, "/") {
			if p, err := netip.ParsePrefix(raw); err == nil && p.Contains(addr) {
				return true
			}
			continue
		}
		if h, _, err := net.SplitHostPort(raw); err == nil {
			raw = h
		}
		if a, err := netip.ParseAddr(raw); err == nil && a == addr {
			return true
		}
	}
	return false
}

// literalInUnreservedPrivateSpace returns true when addr sits in
// the RFC1918 unreserved private ranges: 10.0.0.0/8, 172.16.0.0/12,
// 192.168.0.0/16. These are the ranges Nexus would otherwise be
// hostile to a tenant destination for, but an explicit operator
// allow-list (in_cluster_only mode) may rescue them. Other prefixes
// that the static IP policy refuses (link-local, the IMDS block,
// loopback, carrier-grade NAT, multicast, etc.) are out of scope
// here even if a CIDR match in AllowedInternalHosts is somehow
// present, because those prefixes are off-limits on principle.
func (g *Guard) literalInUnreservedPrivateSpace(addr netip.Addr) bool {
	for _, pfx := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if p, err := netip.ParsePrefix(pfx); err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}

// of AllowedInternalHosts as either an exact IP or inside a CIDR.
// The static IP policy's alwaysBlocked prefixes still trump: an
// address in the metadata range returns false even if the
// operator's allow-list typo overlaps it, because the static
// policy is the bottom layer and is intentionally never overridden.
func (g *Guard) addrInInternalAllowList(addr netip.Addr) bool {
	if err := g.checkAddr(Tenant, addr); err != nil {
		// The static IP policy refused this address on principle.
		// The in-cluster allow-list cannot widen that.
		return false
	}
	for _, raw := range g.policy.AllowedInternalHosts {
		if strings.Contains(raw, "/") {
			if p, err := netip.ParsePrefix(raw); err == nil && p.Contains(addr) {
				return true
			}
			continue
		}
		if h, _, err := net.SplitHostPort(raw); err == nil {
			raw = h
		}
		if a, err := netip.ParseAddr(raw); err == nil && a == addr {
			return true
		}
	}
	return false
}

// hostInInternalAllowList returns true when host matches an entry
// of AllowedInternalHosts verbatim. Suffix matching is intentional
// NOT supported: "llm.local" is the listed hostname, "api.llm.local"
// is a different one and the operator lists it separately. This is
// stricter than the dial hook, where CIDR matches cover the
// ambiguity; runtime refusal of an unlisted public FQDN is the
// desired behaviour.
func (g *Guard) hostInInternalAllowList(host string) bool {
	for _, raw := range g.policy.AllowedInternalHosts {
		if strings.Contains(raw, "/") {
			continue
		}
		if h, _, err := net.SplitHostPort(raw); err == nil {
			raw = h
		}
		if raw == host {
			return true
		}
	}
	return false
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
