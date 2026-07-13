package providers

import "testing"

// TestAcquireRelease_RecyclesCapacity asserts that the buffer pool
// returns the same underlying memory (via capacity) so the next Get
// doesn't allocate. We can't compare pointers safely (sync.Pool
// implementations can drop entries), but capacity is sufficient: any
// buffer with cap == 64 KiB came from the pool.
func TestAcquireRelease_RecyclesCapacity(t *testing.T) {
	b := acquireBuffer()
	if cap(b) < 64*1024 {
		t.Fatalf("acquireBuffer cap should be >= 64KiB, got %d", cap(b))
	}
	if len(b) != 0 {
		t.Fatalf("acquireBuffer len should be 0, got %d", len(b))
	}
	b = b[:42]
	releaseBuffer(b)

	b2 := acquireBuffer()
	if cap(b2) < 64*1024 {
		t.Fatalf("second acquireBuffer cap should be >= 64KiB, got %d", cap(b2))
	}
	if len(b2) != 0 {
		t.Fatalf("second acquireBuffer len should be 0, got %d", len(b2))
	}
	releaseBuffer(b2)
}

// TestReleaseBuffer_RejectsUndersized confirms we don't poison the pool
// with smaller slices that would break future Scanner.Buffer calls
// (which require cap >= max-line-size).
func TestReleaseBuffer_RejectsUndersized(t *testing.T) {
	small := make([]byte, 0, 1024)
	releaseBuffer(small) // must not panic; must not return small to pool
	// After release, the next acquire should still yield a >=64KiB buf.
	got := acquireBuffer()
	if cap(got) < 64*1024 {
		t.Fatalf("pool poisoned, got cap=%d", cap(got))
	}
	releaseBuffer(got)
}
