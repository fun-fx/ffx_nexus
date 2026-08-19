// c0.6 IP source policy: socket address is canonical; headers are
// trusted only through a strict CIDR + hops walk; raw and effective
// values are kept separate; masking options are testable.

package ippolicy

import (
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestResolveSourceSocketByDefault asserts the safe default: when no
// trusted CIDRs are configured, the effective IP is the socket address
// regardless of any header the request carries.
func TestResolveSourceSocketByDefault(t *testing.T) {
	r := &http.Request{
		RemoteAddr: "203.0.113.5:55555",
		Header:     http.Header{},
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	src := ResolveSource(r, nil, 1)
	if src.EffectiveIP != "203.0.113.5" {
		t.Errorf("EffectiveIP = %q, want 203.0.113.5 — headers must NOT be trusted without CIDR", src.EffectiveIP)
	}
	if src.TrustProxyHeader {
		t.Errorf("TrustProxyHeader = true; must be false when CIDR list is empty")
	}
}

// TestResolveSourceRejectsHeaderOutsideTrust asserts an attacker
// who sets X-Forwarded-For without going through a trusted proxy
// cannot influence the effective IP.
func TestResolveSourceRejectsHeaderOutsideTrust(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	r := &http.Request{
		RemoteAddr: "198.51.100.42:1212",
		Header:     http.Header{},
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	src := ResolveSource(r, []*net.IPNet{cidr}, 2)
	if src.EffectiveIP != "198.51.100.42" {
		t.Errorf("EffectiveIP = %q, want socket address 198.51.100.42 — untrusted socket must drop header", src.EffectiveIP)
	}
}

// TestResolveSourceWalksRightmostHopFirst asserts the walk goes
// RIGHT-to-LEFT (the rightmost is the most recently appended by our
// reverse proxy). The chain in the request is `original, proxy1,
// proxy2` and the rightmost is proxy2 — Nexus's immediately-upstream
// proxy. We accept up to trustedHops hops. Test asserts HopsUsed=1
// because the rightmost is in the trust CIDR.
func TestResolveSourceWalksRightmostHopFirst(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	r := &http.Request{
		RemoteAddr: "10.0.0.1:1234",
		Header:     http.Header{},
	}
	r.Header.Set("X-Forwarded-For", "192.168.1.5, 10.0.0.2, 10.0.0.3")
	src := ResolveSource(r, []*net.IPNet{cidr}, 5)
	// Walk right-to-left:
	//   i=2: 10.0.0.3 — trusted → effective=10.0.0.3, accepted=1
	//   i=1: 10.0.0.2 — trusted → effective=10.0.0.2, accepted=2
	//   i=0: 192.168.1.5 — untrusted → break
	// Final: effective=10.0.0.2 (deeper-trusted), accepted=2.
	if src.EffectiveIP != "10.0.0.2" {
		t.Errorf("EffectiveIP = %q, want 10.0.0.2 (leftmost-trusted within hops)", src.EffectiveIP)
	}
	if src.HopsUsed != 2 {
		t.Errorf("HopsUsed = %d, want 2 (rightmost-trusted + leftward trusted)", src.HopsUsed)
	}
}

// TestResolveSourceStopsWalkingOnUntrustedHop is the load-bearing
// safety invariant: once we see a hop OUTSIDE the trusted CIDR, the
// walk stops. The rightmost is `10.0.0.2` (trusted), the leftward
// neighbor `1.2.3.4` is untrusted. Accepted=1 hop, effective = 10.0.0.2.
func TestResolveSourceStopsWalkingOnUntrustedHop(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	r := &http.Request{
		RemoteAddr: "10.0.0.1:1234",
		Header:     http.Header{},
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.2")
	src := ResolveSource(r, []*net.IPNet{cidr}, 5)
	if src.EffectiveIP != "10.0.0.2" {
		t.Errorf("EffectiveIP = %q, want 10.0.0.2 (rightmost trusted)", src.EffectiveIP)
	}
	if src.HopsUsed != 1 {
		t.Errorf("HopsUsed = %d, want 1 (walk stopped at leftward 1.2.3.4)", src.HopsUsed)
	}
}

// TestResolveSourceIPv6HandledBracketAndBare verifies both forms
// parse cleanly without panicking. The rightmost hop is 2001:db8::2
// (RFC 3849 documentation prefix); the leftward is the bracketed
// port variant. We accept 1 hop by default.
func TestResolveSourceIPv6HandledBracketAndBare(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("2001:db8::/32")
	// Socket inside the trust CIDR.
	r := &http.Request{RemoteAddr: "[2001:db8::1]:8080", Header: http.Header{}}
	r.Header.Set("X-Forwarded-For", "[2001:db8::1]:80, 2001:db8::2")
	src := ResolveSource(r, []*net.IPNet{cidr}, 5)
	if src.EffectiveIP != "2001:db8::1" {
		t.Errorf("EffectiveIP = %q, want 2001:db8::1 (leftmost-trusted within hops)", src.EffectiveIP)
	}
}

// TestResolveSourceHeadersOverCapAreRejected asserts a deliberate
// attack pattern — a 100 KiB X-Forwarded-For header — produces a
// Socket-only result and does not panic.
func TestResolveSourceHeadersOverCapAreRejected(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	r := &http.Request{RemoteAddr: "10.0.0.1:1234", Header: http.Header{}}
	overlong := strings.Repeat("10.0.0.2,", 10000)
	r.Header.Set("X-Forwarded-For", overlong)
	src := ResolveSource(r, []*net.IPNet{cidr}, 5)
	if src.EffectiveIP == "" {
		t.Fatalf("header over 8KiB should fall back to socket silently, not panic")
	}
	if src.HopsUsed != 0 {
		t.Errorf("HopsUsed = %d, want 0 (overlong header refused)", src.HopsUsed)
	}
}

// TestMaskedIsPrivacyAwarest is the masking contract: an operator
// can configure --ip-mask-level to keep /24 (V4) or /48 (v6) without
// truncating the IPv6 prefix that distinguishes sites.
func TestMaskedIsPrivacyAware(t *testing.T) {
	cases := map[string]string{
		"203.0.113.5":     "203.0.113.0",
		"10.0.0.42":       "10.0.0.0",
		"2001:db8::abcd":  "2001:db8::",
		"":                "",
		"not-an-ip":       "not-an-ip",
	}
	for in, want := range cases {
		if got := Masked(in); got != want {
			t.Errorf("Masked(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseForwardedForIsLengthBounded verifies we refuse to operate
// on an oversized header even before parsing — attackers should not
// succeed by sending megabytes of XFF.
func TestParseForwardedForIsLengthBounded(t *testing.T) {
	out := ParseForwardedFor(strings.Repeat("1.1.1.1,", 100000))
	if out != nil {
		t.Errorf("ParseForwardedFor on 700KB input should return nil, got %d", len(out))
	}
}
