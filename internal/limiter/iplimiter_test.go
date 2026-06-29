package limiter

import (
	"testing"
	"time"
)

func TestIPLimiterBurstThenDeny(t *testing.T) {
	l := NewIPLimiter(30, time.Minute)
	for i := 0; i < 30; i++ {
		if !l.Allow("k") {
			t.Fatalf("request %d should be allowed (capacity 30)", i+1)
		}
	}
	if l.Allow("k") {
		t.Fatalf("31st request should be denied (capacity exhausted)")
	}
}

func TestIPLimiterPerKeyIsolation(t *testing.T) {
	l := NewIPLimiter(2, time.Minute)
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatalf("a should be allowed twice")
	}
	if l.Allow("a") {
		t.Fatalf("a should be denied after 2")
	}
	if !l.Allow("b") {
		t.Fatalf("b should be allowed; buckets must be independent")
	}
}

func TestIPLimiterRefill(t *testing.T) {
	// Capacity 2 per 1m: 30s in gives 1 token, 60s in gives 2 tokens.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	l := NewIPLimiter(2, time.Minute)
	l.now = func() time.Time { return now }

	if !l.Allow("k") || !l.Allow("k") {
		t.Fatalf("first two should pass")
	}
	if l.Allow("k") {
		t.Fatalf("third should be denied at t=0 (bucket empty)")
	}
	// 60s later: bucket fully refilled (2 tokens).
	now = now.Add(60 * time.Second)
	if !l.Allow("k") || !l.Allow("k") {
		t.Fatalf("after full refill, both should be allowed")
	}
	if l.Allow("k") {
		t.Fatalf("third request in a fresh window should be denied again")
	}
}

func TestIPLimiterDisabled(t *testing.T) {
	l := NewIPLimiter(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !l.Allow("k") {
			t.Fatalf("capacity 0 means unlimited; request %d should pass", i+1)
		}
	}
}
