package limiter

import (
	"context"
	"testing"
	"time"
)

func TestMemoryAllowRPM(t *testing.T) {
	m := NewMemory()
	fixed := time.Date(2026, 6, 2, 10, 30, 0, 0, time.UTC)
	m.now = func() time.Time { return fixed }
	ctx := context.Background()

	// rpmLimit=2: first two allowed, third denied within the same minute.
	for i := 0; i < 2; i++ {
		ok, err := m.Allow(ctx, "k1", 2)
		if err != nil || !ok {
			t.Fatalf("request %d should be allowed (ok=%v err=%v)", i, ok, err)
		}
	}
	if ok, _ := m.Allow(ctx, "k1", 2); ok {
		t.Fatal("third request should be denied")
	}

	// Next minute resets the window.
	fixed = fixed.Add(time.Minute)
	if ok, _ := m.Allow(ctx, "k1", 2); !ok {
		t.Fatal("request in new minute should be allowed")
	}
}

func TestMemoryUnlimitedRPM(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		if ok, _ := m.Allow(ctx, "k", 0); !ok {
			t.Fatalf("rpmLimit=0 must be unlimited, denied at %d", i)
		}
	}
}

func TestMemorySpend(t *testing.T) {
	m := NewMemory()
	fixed := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return fixed }
	ctx := context.Background()

	_ = m.AddSpend(ctx, "k", 1.25)
	_ = m.AddSpend(ctx, "k", 0.75)
	got, _ := m.MonthlySpend(ctx, "k")
	if got != 2.0 {
		t.Fatalf("want 2.0, got %v", got)
	}

	// Different key is isolated.
	if other, _ := m.MonthlySpend(ctx, "other"); other != 0 {
		t.Fatalf("other key should be 0, got %v", other)
	}

	// New month resets.
	fixed = fixed.AddDate(0, 1, 0)
	if next, _ := m.MonthlySpend(ctx, "k"); next != 0 {
		t.Fatalf("new month should reset, got %v", next)
	}
}
