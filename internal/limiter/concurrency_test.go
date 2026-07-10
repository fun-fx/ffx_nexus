package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrencyCap_NilIsNoop(t *testing.T) {
	var c *ConcurrencyCap
	if !c.Acquire(context.Background(), "vk") {
		t.Fatal("nil cap should permit")
	}
	c.Release(context.Background(), "vk")
	if c.Max() != 0 {
		t.Fatalf("nil cap Max: got %d want 0", c.Max())
	}
	if c.Inflight("vk") != 0 {
		t.Fatalf("nil cap Inflight: got %d want 0", c.Inflight("vk"))
	}
}

func TestConcurrencyCap_AcquireRelease(t *testing.T) {
	c := NewConcurrencyCap(3)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if !c.Acquire(ctx, "vk") {
			t.Fatalf("acquire %d should succeed", i)
		}
	}
	if c.Acquire(ctx, "vk") {
		t.Fatal("4th acquire should fail")
	}
	c.Release(ctx, "vk")
	if !c.Acquire(ctx, "vk") {
		t.Fatal("post-release acquire should succeed")
	}
	if c.Inflight("vk") != 3 {
		t.Fatalf("Inflight: got %d want 3", c.Inflight("vk"))
	}
}

func TestConcurrencyCap_DisabledWhenZero(t *testing.T) {
	c := NewConcurrencyCap(0)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if !c.Acquire(ctx, "vk") {
			t.Fatalf("disabled cap should permit, failed at %d", i)
		}
	}
	if c.Max() != 0 {
		t.Fatalf("disabled cap Max: got %d want 0", c.Max())
	}
}

func TestConcurrencyCap_ReleaseUnmatched(t *testing.T) {
	c := NewConcurrencyCap(2)
	c.Release(context.Background(), "vk") // no-op, must not go negative
	if c.Inflight("vk") != 0 {
		t.Fatalf("after unmatched release Inflight: got %d want 0", c.Inflight("vk"))
	}
}

// TestConcurrencyCap_Concurrent hacen hammer the cap from many goroutines
// and assert that the ceiling is never breached, accounting for races.
func TestConcurrencyCap_Concurrent(t *testing.T) {
	const cap = 8
	const goroutines = 64
	const perG = 100
	c := NewConcurrencyCap(cap)
	ctx := context.Background()

	var wg sync.WaitGroup
	var in int64
	var maxSeen int64
	var rejected int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if c.Acquire(ctx, "vk") {
					cur := atomic.AddInt64(&in, 1)
					for {
						prev := atomic.LoadInt64(&maxSeen)
						if cur <= prev || atomic.CompareAndSwapInt64(&maxSeen, prev, cur) {
							break
						}
					}
					atomic.AddInt64(&in, -1)
					c.Release(ctx, "vk")
				} else {
					atomic.AddInt64(&rejected, 1)
				}
			}
		}()
	}
	wg.Wait()

	if atomic.LoadInt64(&maxSeen) > cap {
		t.Fatalf("maxSeen=%d exceeded cap=%d", maxSeen, cap)
	}
	if c.Inflight("vk") != 0 {
		t.Fatalf("Inflight after drain: got %d want 0", c.Inflight("vk"))
	}
	if rejected == 0 {
		t.Fatalf("rejected should be > 0 when goroutines >> cap (got %d)", rejected)
	}
}
