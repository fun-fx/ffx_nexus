package gateway

import (
	"context"
	"testing"
)

// cacheScope decides which tenant's semantic-cache namespace a request reads and
// writes. Getting it wrong returns one org's generated completions to another, so
// the precedence is pinned here rather than left to be inferred from the caller.
//
// internal/semcache/tenancy_test.go proves the cache honours the scope it is
// given. This proves the gateway gives it the right one — both halves are needed,
// because a correctly-scoped cache handed a constant scope still leaks.
func TestCacheScopePrefersTheOrgOverTheVirtualKey(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyOrgID, "org-a")
	ctx = context.WithValue(ctx, ctxKeyVKeyID, "vk-123")

	if got := cacheScope(ctx); got != "org-a" {
		t.Errorf("cacheScope = %q, want the org id. Scoping by virtual key instead "+
			"would fragment the cache per key, but scoping by anything BROADER than "+
			"the org is what leaks across tenants.", got)
	}
}

func TestCacheScopeFallsBackToTheVirtualKeyWhenTheOrgIsUnknown(t *testing.T) {
	// A key issued before orgs existed, or a request authenticated by key alone.
	// The virtual key is narrower than an org, so this cannot widen the scope.
	ctx := context.WithValue(context.Background(), ctxKeyVKeyID, "vk-123")

	if got := cacheScope(ctx); got != "vk-123" {
		t.Errorf("cacheScope = %q, want the virtual key id", got)
	}
}

// The unauthenticated fallback is a single shared namespace. That is correct only
// while there is no tenant to attribute the request to — which is the case when
// gateway auth is disabled, i.e. a single-tenant installation.
//
// Asserted so that a future change routing AUTHENTICATED traffic through this path
// fails here instead of silently merging every org into one namespace.
func TestCacheScopeWithNoTenantIsASingleSharedNamespace(t *testing.T) {
	if got := cacheScope(context.Background()); got != "default" {
		t.Errorf("cacheScope = %q, want %q", got, "default")
	}

	// The empty-string cases must not produce an empty scope, which would make the
	// namespace depend on the key layout rather than on the tenant.
	ctx := context.WithValue(context.Background(), ctxKeyOrgID, "")
	ctx = context.WithValue(ctx, ctxKeyVKeyID, "")
	if got := cacheScope(ctx); got != "default" {
		t.Errorf("cacheScope with empty ids = %q, want %q", got, "default")
	}
}

// Two different orgs must never map to the same scope. Stated as a property so it
// holds for id shapes the current tests do not enumerate.
func TestDistinctOrgsAlwaysMapToDistinctScopes(t *testing.T) {
	orgs := []string{
		"org-a", "org-b",
		"org", "org:a", // colon-bearing ids: these collided in the cache key
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
	}
	seen := map[string]string{}
	for _, org := range orgs {
		scope := cacheScope(context.WithValue(context.Background(), ctxKeyOrgID, org))
		if prev, dup := seen[scope]; dup {
			t.Errorf("orgs %q and %q both map to scope %q", prev, org, scope)
		}
		seen[scope] = org
	}
}
