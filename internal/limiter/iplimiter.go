package limiter

import (
	"sync"
	"time"
)

// IPLimiter is a small in-memory token-bucket rate limiter keyed by an
// arbitrary string (e.g. "auth-login:1.2.3.4"). It is intentionally
// separate from the per-virtual-key Limiter: this one protects unauthenticated
// routes from anonymous abuse, the other protects authenticated spend.
//
// State is per-process. For a multi-replica deployment behind a load
// balancer the limit is per-replica, not global. That is acceptable for v1.1
// (small clusters, small surface area); a Redis-backed variant can be added
// later if abuse shows up.
type IPLimiter struct {
	capacity   int           // max tokens (== max requests per window)
	refill     time.Duration // time to fully refill the bucket
	gcInterval time.Duration // how often to sweep stale buckets
	now        func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewIPLimiter returns a limiter that allows `capacity` requests per `refill`
// window per key. A capacity of 30 with a 1m refill means a steady 30 req/min
// per key with burst of 30.
func NewIPLimiter(capacity int, refill time.Duration) *IPLimiter {
	return &IPLimiter{
		capacity:   capacity,
		refill:     refill,
		gcInterval: refill, // sweep stale buckets at most once per window
		now:        time.Now,
		buckets:    make(map[string]*bucket),
	}
}

// Allow consumes one token for key. Returns true when the request fits
// within the bucket (and thus should proceed), false when it should be
// rejected (e.g. with HTTP 429).
func (l *IPLimiter) Allow(key string) bool {
	if l.capacity <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if since := now.Sub(l.lastGC); since >= l.gcInterval {
		l.gcLocked(now)
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.capacity), last: now}
		l.buckets[key] = b
	}
	// Refill: tokens per second = capacity / refill.Seconds()
	elapsed := now.Sub(b.last).Seconds()
	rate := float64(l.capacity) / l.refill.Seconds()
	b.tokens += elapsed * rate
	if b.tokens > float64(l.capacity) {
		b.tokens = float64(l.capacity)
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gcLocked drops buckets that have not been touched in the last refill
// window. Held under l.mu.
func (l *IPLimiter) gcLocked(now time.Time) {
	cutoff := now.Add(-l.refill)
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}
