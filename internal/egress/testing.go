package egress

import "testing"

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
