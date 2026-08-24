package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
)

// The process-wide guard.
//
// A package-level default is a deliberate choice over threading a *Guard into
// thirteen constructors. The alternative was tried on paper and rejected for a
// specific reason: the conversion has to be a one-line change at each call site,
// or it will be done inconsistently, and the paths most likely to be skipped are
// the ones written under time pressure — which is exactly where the last five
// defects came from. A dependency you must plumb through four layers to reach is
// a dependency people work around.
//
// The safety properties that make this acceptable:
//
//   - The zero state is the SAFE state. Before SetDefault runs, Default returns
//     a guard with the strict policy, so a code path that fires during init or in
//     a test gets private-address blocking rather than no policy.
//   - It is write-once-at-boot, read-many. main calls SetDefault before serving.
//   - It carries policy, not per-request state, so there is nothing for
//     concurrent requests to interfere over.
var defaultGuard atomic.Pointer[Guard]

// SetDefault installs the process guard. Call once from main, before any
// component that may perform egress is started.
func SetDefault(g *Guard) {
	if g == nil {
		return
	}
	defaultGuard.Store(g)
}

// Default returns the process guard, or a strict one if SetDefault has not run.
func Default() *Guard {
	if g := defaultGuard.Load(); g != nil {
		return g
	}
	// Not memoised: the fallback is only hit before boot completes or in tests,
	// and memoising it would let an early caller pin the strict policy in place
	// where SetDefault could not replace it.
	return New(Policy{})
}

// Client returns a guarded client from the process guard. This is the call every
// egress path should use.
func Client(class Class, timeout time.Duration) *http.Client {
	return Default().Client(class, timeout)
}

// Dialer returns a guarded *net.Dialer for non-HTTP callers (SMTP, raw TCP).
// The connect-time address check is identical to the one Client uses; the
// only thing missing is the http.Transport plumbing around it. Connect
// itself is capped at dialTimeout (10 s); longer overall send budgets are
// the caller's job via net.Conn deadlines.
func Dialer(class Class) *net.Dialer {
	return Default().Dialer(class)
}

// CheckURL validates a destination against the process guard. Use it where a URL
// is accepted for storage — a credential's base_url, an eval profile's endpoint,
// a plugin manifest — so the rejection lands on the person configuring it.
func CheckURL(ctx context.Context, rawURL string, class Class) error {
	return Default().CheckURL(ctx, rawURL, class)
}

// CheckConfiguredURL is CheckURL with resolution failures tolerated, which is
// what a configuration validator wants.
//
// It rejects a malformed URL and an address the policy forbids, and it accepts a
// host that simply does not resolve right now. See ErrUnresolvable for why: the
// alternative makes saving a valid vendor URL contingent on the pod's DNS at that
// moment, and the address policy is enforced at dial time regardless.
func CheckConfiguredURL(ctx context.Context, rawURL string, class Class) error {
	err := CheckURL(ctx, rawURL, class)
	if err != nil && errors.Is(err, ErrUnresolvable) {
		return nil
	}
	return err
}

// ParseTenantAllowedCIDRs reads the operator's allowlist.
//
// Accepts a comma-separated list of CIDRs, and for convenience a bare address,
// which becomes a /32 or /128. Returns an error naming the bad entry rather than
// skipping it: an operator who typo'd the range meant to allow something, and
// silently allowing nothing would present as "the plugin never reports scores".
func ParseTenantAllowedCIDRs(raw string) ([]netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if p, err := netip.ParsePrefix(field); err == nil {
			out = append(out, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(field)
		if err != nil {
			return nil, &InvalidCIDRError{Entry: field}
		}
		bits := 32
		if addr.Is6() {
			bits = 128
		}
		out = append(out, netip.PrefixFrom(addr, bits))
	}
	return out, nil
}

// InvalidCIDRError names the offending entry without dumping the whole list into
// a log line.
type InvalidCIDRError struct{ Entry string }

func (e *InvalidCIDRError) Error() string {
	return "egress: " + e.Entry + " is not a valid CIDR or IP address"
}
