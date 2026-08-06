package console

import (
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// TestProviderStatsCache_GetMissReturnsFalseEmpty verifies the cache miss
// contract used by the /api/stats/providers handler: no entry was set
// under our cache key, so Get returns (nil, false).
func TestProviderStatsCache_GetMissReturnsFalseEmpty(t *testing.T) {
	providerStatsCacheMu.Lock()
	providerStatsCache = map[string]providerStatsCacheEntry{}
	providerStatsCacheMu.Unlock()

	got, ok := providerStatsCacheGet("admin|1h0m0s")
	if ok {
		t.Fatalf("expected cache miss, got value: %#v", got)
	}
	if got != nil {
		t.Fatalf("expected nil value on miss, got %#v", got)
	}
}

// TestProviderStatsCache_RoundTrip pins the happy path: Set then Get returns
// the value within the TTL window. After TTL elapses the entry must be
// considered expired and not returned.
func TestProviderStatsCache_RoundTrip(t *testing.T) {
	providerStatsCacheMu.Lock()
	providerStatsCache = map[string]providerStatsCacheEntry{}
	providerStatsCacheMu.Unlock()

	want := []observability.ProviderStat{
		{Provider: "openai", Requests: 100, CostUSD: 12.34, InputTokens: 2500, OutputTokens: 1300, AvgLatencyMs: 401.2, CacheHits: 12},
		{Provider: "grid", Requests: 50, CostUSD: 7.5, InputTokens: 1800, OutputTokens: 800, AvgLatencyMs: 530, CacheHits: 4},
	}

	key := "user:abc|1h0m0s"

	// Stub the TTL long enough that the two assertions below both see the
	// entry as fresh; we restore the package-level value at the end so
	// other tests are unaffected by the override.
	providerStatsCacheMu.Lock()
	original := providerStatsTTL
	providerStatsTTL = time.Second
	providerStatsCacheMu.Unlock()
	t.Cleanup(func() {
		providerStatsCacheMu.Lock()
		providerStatsTTL = original
		providerStatsCacheMu.Unlock()
	})

	providerStatsCacheSet(key, want)

	// First read: hit.
	got, ok := providerStatsCacheGet(key)
	if !ok {
		t.Fatalf("expected cache hit, got miss")
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Provider != want[i].Provider {
			t.Errorf("[%d].Provider = %q, want %q", i, got[i].Provider, want[i].Provider)
		}
		if got[i].CostUSD != want[i].CostUSD {
			t.Errorf("[%d].CostUSD = %v, want %v", i, got[i].CostUSD, want[i].CostUSD)
		}
	}

	// Second read after TTL elapses: miss.
	time.Sleep(1100 * time.Millisecond)
	got2, ok2 := providerStatsCacheGet(key)
	if ok2 {
		t.Fatalf("expected expiry miss, got value: %#v", got2)
	}
}

// TestProviderStatsCache_KeysDoNotCollide verifies that admin and per-user
// scope keys land in separate buckets. A regression here would silently
// leak one user's provider stats to an admin's Overview tab or vice versa.
func TestProviderStatsCache_KeysDoNotCollide(t *testing.T) {
	providerStatsCacheMu.Lock()
	providerStatsCache = map[string]providerStatsCacheEntry{}
	providerStatsCacheMu.Unlock()

	admin := []observability.ProviderStat{{Provider: "openai", Requests: 1, CostUSD: 0.01}}
	userA := []observability.ProviderStat{{Provider: "anthropic", Requests: 2, CostUSD: 0.02}}
	userB := []observability.ProviderStat{{Provider: "gemini", Requests: 3, CostUSD: 0.03}}

	providerStatsCacheSet("admin|1h0m0s", admin)
	providerStatsCacheSet("user:alice|1h0m0s", userA)
	providerStatsCacheSet("user:bob|1h0m0s", userB)

	gotAdmin, _ := providerStatsCacheGet("admin|1h0m0s")
	gotAlice, _ := providerStatsCacheGet("user:alice|1h0m0s")
	gotBob, _ := providerStatsCacheGet("user:bob|1h0m0s")

	if len(gotAdmin) != 1 || gotAdmin[0].Provider != "openai" {
		t.Errorf("admin bucket poisoned: %#v", gotAdmin)
	}
	if len(gotAlice) != 1 || gotAlice[0].Provider != "anthropic" {
		t.Errorf("alice bucket poisoned: %#v", gotAlice)
	}
	if len(gotBob) != 1 || gotBob[0].Provider != "gemini" {
		t.Errorf("bob bucket poisoned: %#v", gotBob)
	}
}
