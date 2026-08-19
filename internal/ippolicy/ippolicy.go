// Package ippolicy decides what "source IP" means for an HTTP request
// to the Nexus console / gateway. The contract is documented in
// docs/ip-exposure.md; in short:
//
//   - We always capture the raw socket address (RemoteAddr) into the
//     audit row's `request_ip` column. This is the forensically
//     trusted value.
//
//   - "Effective source IP" is socket address by default; the
//     customer-facing X-Forwarded-For / X-Real-IP / Forwarded value
//     is only trusted when both:
//        1. The socket address is in `trustedProxyCIDRs`, AND
//        2. We are still inside the configured `trustedProxyHops`
//           count as we walk LEFT-to-RIGHT through X-Forwarded-For.
//     The first hop that fails the check stops the walk — anything
//     past that point is attacker-controlled.
//
//   - IPv6 addresses must work end-to-end (they do today) but the
//     parser must also accept bracketed IPv6 (`[::1]:80`) without
//     crashing.
//
//   - Headers longer than the configured safety cap (default 8 KiB)
//     are rejected; the helper returns the socket address instead of
//     the offending header.
//
// Masking options (`--ip-mask`) are configurable via Helm values and
// covered by the integration test below.

// Package ippolicy.
package ippolicy

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/ffxnexus/nexus/internal/apierr"
)

// Source captures the deconstructed input we use to derive an
// effective source IP. The struct is exported so callers can persist
// both the raw socket and the (possibly header-derived) effective IP
// to the same audit row.
//
// RawSocketAddr is always the literal RemoteAddr string — including
// the port. We store it verbatim and mask it when the configured
// retention policy says so; the audit row keeps it as a forensic
// anchor regardless.
type Source struct {
	RawSocketAddr    string
	RawSocketHost    string   // host portion after SplitHostPort, no port
	TrustProxyHeader bool     // whether the proxy chain is trusted for header reading
	HopsUsed         int      // how many hops were parsed successfully
	HeaderChain      []string // the parsed hops in order
	EffectiveIP      string   // the final effective source IP
}

// ResolveTakesConfig returns true when the request's RemoteAddr falls
// inside at least one of the configured trusted CIDRs. An empty
// TrustedCIDRs list means "never trust headers", which is the safe
// default — operators may opt in by providing their egress CIDRs
// (e.g. Cloudflare or internal load balancer IPs).
func TrustedRemote(host string, cidrList []*net.IPNet) bool {
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, c := range cidrList {
		if c == nil {
			continue
		}
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// HeaderMaxLen is the per-header cap. Why 8KiB and not 256: a
// legitimate routing layer can stack a couple of IPs and the
// original host header alongside a CSP/TLS tag — 8 KiB is a comfortable
// upper bound that still proves the header was not absurd. A burst
// of MB-sized headers is the attack pattern.
const HeaderMaxLen = 8 * 1024

// ParseForwardedFor parses an X-Forwarded-For header value into a
// slice of (ip, port-or-empty) strings. The header can be spam; the
// caller should drop headers above HeaderMaxLen first.
//
// The parser tolerates:
//   - IPv4 host: 10.0.0.1, with optional port (10.0.0.1:1234)
//   - IPv6 host: 2001:db8::1, with optional bracketed port ([2001:db8::1]:1234)
//   - Trailing whitespace
//
// "unknown"-style junk (e.g. "masked") is captured as a continue
// marker; the walk logic below skips hops that aren't valid IPs.
func ParseForwardedFor(value string) []string {
	if len(value) > HeaderMaxLen {
		// Refuse to even attempt to parse a too-long header. The
		// caller treats this as "no usable proxy chain".
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Strip optional port. IPv6 may be bracketed.
		if strings.HasPrefix(p, "[") {
			if h, _, err := net.SplitHostPort(p); err == nil {
				p = h
			}
		} else if h, _, err := net.SplitHostPort(p); err == nil {
			p = h
		}
		out = append(out, p)
	}
	return out
}

// ResolveSource returns the source-IP deconstruction for a request.
//
// When trustedCIDRs is empty OR the socket address is NOT in any
// trusted CIDR, the function returns a Source whose EffectiveIP is
// the socket address and whose HeaderChain is empty. We deliberately
// never trust X-Forwarded-For when the upstream is uncontrolled — a
// single adversary setting X-Forwarded-For to bypass a per-IP
// permit-list would otherwise be enough to subvert rate limit and
// audit attribution.
//
// When the socket is trusted, we walk the X-Forwarded-For chain
// left-to-right, accepting up to trustedHops hops. The first hop
// whose IP is invalid, or whose IP is not in trustedCIDRs (after the
// first hop — i.e. beyond the trust boundary), causes the walk to
// stop. The effective IP is the leftmost *usable* IP within the
// configured hop limit.
//
// The walk stops for safety on two failure modes:
//   - We exhaust trustedHops before reaching end-of-chain. The
//     effective IP is the last accepted hop.
//   - A hop is outside the trust boundary. Rejecting the chain
//     beyond that hop is conservative; an attacker who can rewrite
//     a hop past the trust boundary has no way to make Nexus act on
//     it.
func ResolveSource(r *http.Request, trustedCIDRs []*net.IPNet, trustedHops int) Source {
	rawAddr := ""
	rawHost := ""
	if r.RemoteAddr != "" {
		rawAddr = r.RemoteAddr
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			rawHost = host
		} else {
			// Many callers (tests) pass a bare host; treat it as
			// already-stripped.
			rawHost = r.RemoteAddr
			rawAddr = r.RemoteAddr + ":0"
		}
	}
	src := Source{
		RawSocketAddr: scrubbed(rawAddr),
		RawSocketHost: scrubbed(rawHost),
	}
	if len(trustedCIDRs) == 0 || trustedHops <= 0 || rawHost == "" {
		src.EffectiveIP = src.RawSocketHost
		return src
	}
	if !TrustedRemote(rawHost, trustedCIDRs) {
		src.EffectiveIP = src.RawSocketHost
		return src
	}
	src.TrustProxyHeader = true
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		src.EffectiveIP = src.RawSocketHost
		return src
	}
	hops := ParseForwardedFor(xff)
	src.HeaderChain = hops
	// Right-to-left walk: the rightmost append is the hop our reverse
	// proxy did (we trust its address because the socket is in the
	// trusted CIDRs). Walking one or more hops toward the leftmost
	// gives us the upstream proxy's trust boundary; the leftmost is
	// the original client (further steps would surface an
	// attacker-controlled hop if any untrusted proxy was on the
	// path).
	//
	// Each accepted hop must satisfy trustedCIDRs; the first failure
	// stops the walk. The trade-off favours safety: we surface the
	// rightmost-trusted hop rather than the leftmost (which an
	// attacker can spoof by inserting their own X-Forwarded-For).
	accepted := 0
	var effective string
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			break
		}
		if !TrustedRemote(hops[i], trustedCIDRs) {
			break
		}
		effective = hops[i]
		accepted++
		if accepted >= trustedHops {
			break
		}
	}
	if effective == "" {
		effective = src.RawSocketHost
	}
	src.HopsUsed = accepted
	src.EffectiveIP = effective
	return src
}

// Masked returns the IP with the host byte replacements. The
// default mask puts /24 granularity on IPv4 (last byte zeroed) and
// /48 on IPv6 (last 80 bits zeroed). Operators may configure
// different policies via Helm values (--ip-mask-level=strict|loose).
func Masked(ip string) string {
	if ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		v4[3] = 0
		return v4.String()
	}
	v6 := parsed.To16()
	if v6 == nil {
		return ip
	}
	for i := 6; i < 16; i++ {
		v6[i] = 0
	}
	return v6.String()
}

// scrubbed is a tiny helper that runs apierr.Scrub against derived
// fields — defence in depth: callers may store them in detail strings
// and we want the same redaction policy applied.
func scrubbed(s string) string {
	return apierr.Scrub(s)
}

// HopsFromConfig parses the integer config value, defaulting to 1.
// Negative values are treated as 0 (no proxy hops trusted).
func HopsFromConfig(s string) int {
	if s == "" {
		return 1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
