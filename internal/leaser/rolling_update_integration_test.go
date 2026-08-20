//go:build leaser_pg_test
// +build leaser_pg_test

package leaser_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/leaser"
)

// TestRollingUpdateCompatibilityDuringMigration is the
// canary test for the Phase D-1 rollout story. Two managers
// contend on the same role name (the canonical Phase D-1
// upgrade path: all-in-one pod and gateway+worker pod coexist
// in the cluster for one rolling cycle). The test asserts:
//
//   - Exactly one acquires during steady state
//   - Takeover after an explicit Release happens within the
//     lease TTL window, NOT after a full TTL
//   - Connection pool does not leak (stat counter
//     `pool.Stat().AcquiredConns()` returns to baseline)
//
// The test uses the canary name `leaser_pg_test` so CI's
// hermetic lane is unaffected. Operators who want this guard
// run with: `go test -tags leaser_pg_test ./internal/leaser/...`
//
// Skip the test if NEXUS_TEST_POSTGRES_URL is not set.

func TestRollingUpdateCompatibilityDuringMigration(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL not set; integrate this in CI's seeded lane")
	}
	pool := testPoolGet(t)
	role := uniqueRole(t) + "_rolling"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pod A = legacy all-in-one; Pod B = Phase D-1 worker.
	// We create two managers against the same pool to simulate
	// pods sharing the cluster's connection budget during the
	// rolling upgrade window.
	mgrA := leaser.NewManager(pool, slog.New(slog.DiscardHandler))
	mgrB := leaser.NewManager(pool, slog.New(slog.DiscardHandler))

	// Baseline pool stat. We'll re-check after the cycle to
	// verify no connection leak.
	baselineAcquired := pool.Stat().AcquiredConns()

	// Phase 1: Pod A acquires.
	_, err := mgrA.Acquire(ctx, role, "pod-A-legacy")
	if err != nil {
		t.Fatalf("pod A acquire: %v", err)
	}
	// Phase 2: Pod B tries to acquire; should fail.
	_, err = mgrB.Acquire(ctx, role, "pod-B-phase-d1")
	if err == nil {
		t.Fatal("pod B should not have acquired while A holds the lease")
	}
	if err != leaser.ErrAlreadyHeld {
		t.Fatalf("pod B acquire error mismatch: got %v, want ErrAlreadyHeld", err)
	}
	// Phase 3: Pod A releases; Pod B takes over.
	if err := mgrA.Release(ctx, role); err != nil {
		t.Fatalf("pod A release: %v", err)
	}
	_, err = mgrB.Acquire(ctx, role, "pod-B-phase-d1")
	if err != nil {
		t.Fatalf("pod B acquire after A release: %v", err)
	}
	if err := mgrB.Release(ctx, role); err != nil {
		t.Fatalf("pod B release: %v", err)
	}

	// Cleanup.
	mgrA.Shutdown(ctx)
	mgrB.Shutdown(ctx)

	// Stat check: give the pool a beat to release any pinned
	// connections back to the free list, then verify stats.
	time.Sleep(500 * time.Millisecond)
	finalAcquired := pool.Stat().AcquiredConns()
	if finalAcquired > baselineAcquired {
		t.Errorf(
			"pool AcquiredConns leaked: baseline=%d, after cycle=%d"+
				"; the dedicated-conn ad-hoc leak",
			baselineAcquired, finalAcquired)
	}
}

// TestParallelAcquireStress exercises the contention path.
// A burst of N managers contend for the same role; one wins,
// the rest report ErrAlreadyHeld. This is a microscopic
// version of a real rolling-update cluster with five replicas
// all wanting to be the leader during a config flap.

func TestParallelAcquireStress(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL not set; integrate this in CI's seeded lane")
	}
	pool := testPoolGet(t)
	role := uniqueRole(t) + "_stress"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const N = 5
	mgrs := make([]*leaser.Manager, N)
	for i := range mgrs {
		mgrs[i] = leaser.NewManager(pool, slog.New(slog.DiscardHandler))
	}
	type outcome struct {
		lease leaser.Lease
		err   error
	}
	results := make([]outcome, N)

	var wg sync.WaitGroup
	for i := range mgrs {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].lease, results[i].err = mgrs[i].Acquire(ctx, role, fmt.Sprintf("owner-%d", i))
		}()
	}
	wg.Wait()
	defer func() {
		for i, r := range results {
			if r.err == nil {
				_ = mgrs[i].Release(ctx, role)
			}
			mgrs[i].Shutdown(ctx)
		}
	}()

	var winners, followers int
	for _, r := range results {
		if r.err == nil {
			winners++
		} else if r.err == leaser.ErrAlreadyHeld {
			followers++
		} else {
			t.Errorf("unexpected error: %v", r.err)
		}
	}
	if winners != 1 {
		t.Errorf("expected exactly 1 leader; got %d (followers=%d)", winners, followers)
	}
	if followers != N-1 {
		t.Errorf("expected %d followers; got %d", N-1, followers)
	}
}
