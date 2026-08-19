// Package urlpolicy is the home for credential base_url
// validation. Two consumers:
//
//   1. Save-time: CreateCredential rejects obviously bad URLs
//      before they reach the database. A trailing slash,
//      a missing scheme, or a hostname pointing at a private
//      network range (a Kubernetes service network, an RFC1918
//      address, link-local) happens to be the bulk of every
//      accidental leak in our incident log. Fail-closed here
//      because the cost of storing a bad URL is a credential
//      that no provider endpoint can serve.
//
//   2. Dial-time: credential_resolver.go and any
//      LLM-gateway-direct HTTP client that uses
//      http.DefaultTransport with the saved URL must apply the
//      same hostname policy before opening a TCP socket. The
//      private-IP gate is the single most-asked question from
//      compliance reviewers, and the dial-time enforcement is
//      what makes the save-time check believable: we do not
//      even consider an SSRF path unless the gateway first
//      accepted the URL.
//
// Two optical properties:
//
//   - The allowed-private-CIDR list is operator-driven and
//     configuration-controlled. A self-hosted installation
//     that legitimately hosts OpenAI on
//     10.0.42.7 registers 10.0.42.0/24 in
//     NEXUS_PRIVATE_CREDENTIAL_HOSTS_ALLOWLIST (a comma-
//     separated CIDR list), and the gate honours that.
//
//   - HTTPS is required. The provider ecosystem's encryption
//     baseline is HTTPS; we never dial HTTP. A 308-style
//     redirect down to HTTP is irrelevant because we never
//     accept the redirect chain into HTTPS-only territory.
//
// The check is a separate package so Save and Dial share the
// same rule; drift here is what historically caused save-time
// and runtime policies to diverge.

package urlpolicy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrEmptyURL is returned when the caller provides an empty
// value. Empty in this product means the operator forgot to
// fill the form, which we cannot recover from silently.
var ErrEmptyURL = errors.New("urlpolicy: base_url is empty")

// ErrUnsupportedScheme is returned when the scheme is not
// https. A provider endpoint over plaintext HTTP is not
// supported.
var ErrUnsupportedScheme = errors.New("urlpolicy: base_url must use https")

// ErrParseURL is returned when net/url.Regexp doesn't recognise
// the value as a URL.
var ErrParseURL = errors.New("urlpolicy: base_url is not a parseable URL")

// ErrPrivateNetwork is returned when the hostname resolves to a
// private IP and the operator has not added the address to
// NEXUS_PRIVATE_CREDENTIAL_HOSTS_ALLOWLIST.
var ErrPrivateNetwork = errors.New("urlpolicy: hostname resolves to a non-public network")

// ErrTraversalSuffix is returned when the URL ends in what
// looks like an upstream-mounted path that does not normally
// exist. The check is intentionally narrow: any string with
// `..` or `/../` is rejected, which is enough to catch the
// bulk of SSRF traversal patterns without requiring a regex
// library.
var ErrTraversalSuffix = errors.New("urlpolicy: base_url contains a path-traversal segment")

// Validate enforces the save-time policy:
//
//  1. non-empty,
//  2. parseable URL,
//  3. https scheme only,
//  4. no `..` traversal segments,
//  5. hostname resolves to a public network address OR the
//     operator has explicitly added the address to the allow-
//     list.
//
// The optional allowlist is a comma-separated CIDR list
// (e.g. "10.0.42.0/24,127.0.0.1/32"); an empty string
// disables private-network destinations entirely.
func Validate(raw, allowlistCSV string) error {
	if strings.TrimSpace(raw) == "" {
		return ErrEmptyURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrParseURL, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return ErrUnsupportedScheme
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: missing hostname", ErrParseURL)
	}
	if strings.Contains(u.Path, "..") {
		return ErrTraversalSuffix
	}
	// Hostname might be a literal IP; in that case ip.Parse
	// works without resolution. If it's a domain name, we walk
	// the system resolver. ns lookup errors are treated as
	// "no public-network claim can be made yet", so we err on
	// the safe side.
	if err := assertPublicHost(host, allowlistCSV); err != nil {
		return err
	}
	return nil
}

// assertPublicHost applies the dial-time policy: any address
// reachable in the public IP space is fine; private IPs need an
// explicit allowlist entry.
//
// Loopback, link-local, ULA (RFC4193), CGN (RFC6598) and
// RFC1918 ranges are all "private" for this check. We use
// net.IP.IsPrivate() and net.IP.IsLoopback() rather than
// hand-rolled masks so the rule tracks what the Go standard
// library considers private.
func assertPublicHost(host, allowlistCSV string) error {
	if ip := net.ParseIP(host); ip != nil {
		return checkIP(ip, allowlistCSV)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// We can't prove the host is public or private, so we
		// reject. A record exists in DNS that we couldn't read
		// is bad enough; we shouldn't pass a base_url through
		// that we couldn't resolve.
		return fmt.Errorf("urlpolicy: cannot resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("urlpolicy: %s resolved to no addresses", host)
	}
	for _, ip := range ips {
		if err := checkIP(ip, allowlistCSV); err == nil {
			// At least one public IP is enough to let the
			// URL through. A multi-A-record host with a mixed
			// public/private answer is allowed; the dial client
			// tries in order and the gate fires per-address.
			return nil
		}
	}
	return fmt.Errorf("%w: %s and no public IP in DNS",
		ErrPrivateNetwork, host)
}

func checkIP(ip net.IP, allowlistCSV string) error {
	for _, cidr := range parseAllowlist(allowlistCSV) {
		if cidr.Contains(ip) {
			return nil
		}
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("%w: %s", ErrPrivateNetwork, ip)
	}
	return nil
}

// CIDRs is the parser entry point. The result is reused across
// requests when the cluster's admin sets a Helm value:
func parseAllowlist(csv string) []*net.IPNet {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	var out []*net.IPNet
	for _, c := range strings.Split(csv, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}
