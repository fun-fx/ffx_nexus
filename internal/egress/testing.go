package egress

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
)

// This file exists because the guard's correct behaviour is inconvenient for
// tests in exactly one way: httptest servers listen on 127.0.0.1, and a
// tenant-class destination on loopback is precisely what the guard refuses.
//
// The tempting fix — quietly relaxing the policy when running under `go test` —
// would disable the control in the tests that are supposed to verify it. So the
// relaxation is explicit, per-package, requires a testing.TB, and restores the
// previous guard on cleanup. Anything that relaxes the policy therefore appears
// in a diff and in a grep.
//
// The policy tests in this package deliberately do NOT use these helpers.

// Policy returns the policy the guard was constructed with. Used
// in tests where the env-var → Policy wiring is the unit under
// test (the call sites of installEgressGuard, where reading the
// field directly would expose internals).
func (g *Guard) Policy() Policy {
	if g == nil {
		return Policy{}
	}
	return g.policy
}

// TestingAllowLoopback lets tenant-class destinations reach loopback for the
// duration of tb.
//
// Use it in packages whose tests point vendor adapters at an httptest server to
// verify payload encoding, headers or error handling — behaviour that has nothing
// to do with destination policy. Do NOT use it in a test that asserts something
// about which destinations are permitted; such a test would then be verifying the
// relaxed policy and would pass with the guard removed entirely.
func TestingAllowLoopback(tb testing.TB) {
	tb.Helper()
	restoreGuard(tb)
	SetDefault(New(loopbackPolicy()))
}

// TestingStrict installs the default strict policy for the duration of tb, and
// restores whatever was there before.
//
// Use it inside a package that has relaxed the policy in TestMain when you need
// one test to see production behaviour — asserting that a tenant-supplied
// base_url cannot reach a private address, for example. Without this the test
// would run under the package-wide relaxation and prove nothing.
func TestingStrict(tb testing.TB) {
	tb.Helper()
	restoreGuard(tb)
	SetDefault(New(Policy{}))
}

// restoreGuard registers the cleanup that puts the previous guard back, so tests
// in the same package do not inherit each other's policy.
func restoreGuard(tb testing.TB) {
	prev := defaultGuard.Load()
	tb.Cleanup(func() {
		if prev == nil {
			defaultGuard.Store(nil)
			return
		}
		defaultGuard.Store(prev)
	})
}

// AllowLoopbackForPackageTests relaxes the policy for a whole package from
// TestMain, where no testing.TB exists yet.
//
// Prefer TestingAllowLoopback. Reach for this only when a package has many tests
// that point adapters at httptest servers and threading the helper through each
// one would bury the change. A package that calls this must be listed in the
// comment on internal/egress/inventory_test.go so the relaxation is discoverable
// from the guard's own tests.
func AllowLoopbackForPackageTests() {
	SetDefault(New(loopbackPolicy()))
}

// loopbackPolicy is the only way allowLoopback is ever set. Keeping it in one
// unexported function means a grep for allowLoopback finds every relaxation.
func loopbackPolicy() Policy {
	return Policy{allowLoopback: true}
}

// TestingSetResolver replaces g's hostname resolver with `table`,
// where table maps an FQDN to a fixed list of literal IP answers,
// and returns a Cleanup that restores the previous resolver.
//
// Use it in tests that have to exercise the in_cluster_only or
// proxy modes without depending on net.DefaultResolver, which is
// not part of the unit under test and has been observed to
// behave inconsistently across CI runner images (systemd-resolved
// returns "server misbehaving" on certain cluster.local queries,
// for example). This helper keeps the "the unit" surface smaller.
func TestingSetResolver(tb testing.TB, g *Guard, table map[string][]string) func() {
	tb.Helper()
	if g == nil {
		tb.Fatal("TestingSetResolver called with nil guard")
	}
	prev := g.resolver
	g.resolver = func(_ context.Context, host string) ([]netip.Addr, error) {
		raw, ok := table[host]
		if !ok {
			return nil, fmt.Errorf("test resolver: no entry for host: %s", host)
		}
		out := make([]netip.Addr, 0, len(raw))
		for _, r := range raw {
			out = append(out, netip.MustParseAddr(r))
		}
		return out, nil
	}
	return func() { g.resolver = prev }
}
