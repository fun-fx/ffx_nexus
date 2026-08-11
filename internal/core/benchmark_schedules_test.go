package core

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// usingTestDB grabs NEXUS_TEST_POSTGRES_URL or skips. We mirror the
// pattern core's other tests already use so a contributor build without
// Postgres does not have to think about it.
func usingTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// A unique per-test prefix keeps test rows from colliding across runs
// that share one database. Resetting the prefix is not necessary
// because tests are pairwise-cleaned before each teardown.
var schedulePrefixCounter uint64

func newScheduleID(t *testing.T) string {
	t.Helper()
	n := atomic.AddUint64(&schedulePrefixCounter, 1)
	return "schd-" + t.Name()[:min(6, len(t.Name()))] + "-" + time.Now().UTC().Format("150405") + "-" + uintToStr(n)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func uintToStr(n uint64) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		d := n % 10
		out = string(rune('0'+d)) + out
		n /= 10
	}
	return out
}

func TestBenchmarkScheduleCreateAndList(t *testing.T) {
	pool := usingTestDB(t)
	store := &Store{pool: pool}
	ctx := context.Background()

	id := newScheduleID(t)
	row := BenchmarkSchedule{
		ID:             id,
		OrgID:          "org-test",
		Name:           "daily-gsm8k",
		Environments:   []string{"primeintellect/gsm8k"},
		Model:          "openai/gpt-4o-mini",
		NumExamples:    5,
		Rollouts:       1,
		ViaGateway:     true,
		CadenceSeconds: 86400,
		Enabled:        true,
		CreatedBy:      "test",
	}
	if err := store.CreateBenchmarkSchedule(ctx, row); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteBenchmarkSchedule(ctx, id) })

	got, err := store.GetBenchmarkSchedule(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CadenceSeconds != 86400 {
		t.Fatalf("cadence: want 86400, got %d", got.CadenceSeconds)
	}
	if !got.NextLaunchAt.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("next_launch_at not initialised: %v", got.NextLaunchAt)
	}

	t.Cleanup(func() { _ = store.DeleteBenchmarkSchedule(ctx, id) })
}
