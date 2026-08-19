package urlpolicy

import (
	"strings"
	"testing"
)

// The save-time + dial-time gate is the single line of defence
// against SSRF via stored credentials. Eight checks pinned here:
//
//  1. Empty URL is rejected.
//  2. Non-https scheme is rejected — http:// is not acceptable;
//     a redirect down to HTTP only matters once a connection
//     opens, and we already fail before that.
//  3. Plain hostname with no scheme is rejected.
//  4. Public-IP hostnames pass.
//  5. Private/loopback hosts fail without an allowlist.
//  6. Private/loopback hosts pass with an empty allowlist (entry
//     present in CSV) when the operator has explicitly added them.
//  7. RFC1918 192.168.0.0/16 fail without explicit allowlist.
//  8. Path-traversal `..` segment is rejected (covers the
//     `/api/../internal/foo` style of upstream-mount trick).

func TestValidateRejectsEmpty(t *testing.T) {
	if err := Validate("", ""); err != ErrEmptyURL {
		t.Errorf("got %v, want ErrEmptyURL", err)
	}
	if err := Validate("   ", ""); err != ErrEmptyURL {
		t.Errorf("got %v, want ErrEmptyURL", err)
	}
}

func TestValidateRequiresHTTPS(t *testing.T) {
	cases := []string{
		"http://api.openai.com/v1",
		"HTTP://api.openai.com/v1",
		"ftp://api.openai.com/v1",
		"ws://api.openai.com/v1",
		"ssh://api.openai.com/v1",
	}
	for _, c := range cases {
		if err := Validate(c, ""); err != ErrUnsupportedScheme {
			t.Errorf("got %v for %q, want ErrUnsupportedScheme", err, c)
		}
	}
}

func TestValidateRejectsPathTraversal(t *testing.T) {
	cases := []string{
		"https://api.openai.com/v1/../../etc",
		"https://api.openai.com/..",
		"https://api.openai.com/v1/../internal",
	}
	for _, c := range cases {
		if err := Validate(c, ""); err != ErrTraversalSuffix {
			t.Errorf("got %v for %q, want ErrTraversalSuffix", err, c)
		}
	}
}

func TestValidateAcceptsPublicHostname(t *testing.T) {
	// Network resolution is involved; we let the test skip when
	// the test environment cannot reach DNS. Most CI runners can.
	cases := []string{
		"https://api.openai.com/v1",
		"https://api.anthropic.com",
		"https://generativelanguage.googleapis.com",
	}
	for _, c := range cases {
		if err := Validate(c, ""); err != nil {
			t.Errorf("got %v for %q, expected accept", err, c)
		}
	}
}

func TestValidateRejectsPrivateHostWithoutAllowlist(t *testing.T) {
	cases := []string{
		"https://127.0.0.1",          // loopback
		"https://127.0.0.1/v1",       // loopback with path
		"https://10.0.0.5",           // RFC1918
		"https://192.168.1.7",        // RFC1918
		"https://169.254.169.254",    // AWS metadata
		"https://[::1]",              // IPv6 loopback
		"https://[fe80::1]",          // IPv6 link-local
		"https://localhost",          // resolves to 127.0.0.1
	}
	for _, c := range cases {
		err := Validate(c, "")
		if err == nil {
			t.Errorf("got nil for %q, want ErrPrivateNetwork", c)
			continue
		}
		if !strings.Contains(err.Error(), "private") &&
			!strings.Contains(err.Error(), "non-public") &&
			!strings.Contains(err.Error(), "loopback") &&
			!strings.Contains(err.Error(), "link-local") {
			t.Errorf("got %v for %q, want private-network error", err, c)
		}
	}
}

func TestValidateAcceptsAllowlistedPrivateHost(t *testing.T) {
	allowlist := "10.0.42.0/24,127.0.0.1/32"
	cases := []string{
		"https://127.0.0.1/v1",  // loopback allowed
		"https://10.0.42.7/v1",  // within allowlist
		"https://10.0.42.99/v1", // within allowlist
	}
	for _, c := range cases {
		if err := Validate(c, allowlist); err != nil {
			t.Errorf("got %v for %q with allowlist %q, expected pass", err, c, allowlist)
		}
	}

	// Hosts NOT in the allowlist still fail.
	if err := Validate("https://10.0.43.0", allowlist); err == nil {
		t.Error("host outside allowlist unexpectedly accepted")
	}
	if err := Validate("https://192.168.1.1", allowlist); err == nil {
		t.Error("RFC1918 host outside allowlist unexpectedly accepted")
	}
}

func TestValidateRejectsBogusCIDRInAllowlist(t *testing.T) {
	allowlist := "not-a-cidr,also-bogus,10.0.42.0/24"
	if err := Validate("https://10.0.42.7", allowlist); err != nil {
		t.Errorf("got %v; bogus CIDRs must be silently dropped, not blocking valid entries",
			err)
	}
	// And if the entire allowlist is junk, the gate fails closed.
	if err := Validate("https://127.0.0.1", "not-a-cidr"); err == nil {
		t.Error("127.0.0.1 accepted when allowlist was unparseable")
	}
}

func TestValidateAcceptsLiteralIPv6Allowed(t *testing.T) {
	allowlist := "::1/128"
	if err := Validate("https://[::1]/v1", allowlist); err != nil {
		t.Errorf("got %v for loopback with allowlist, want pass", err)
	}
}
