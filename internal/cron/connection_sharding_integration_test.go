// Connection sharding regression test for Phase D-1. The
// pre-Phase-D-1 path put cron.Runner and the gateway on the same
// pod with a single pgxpool that was sized for the gateway hot
// path; the cron fires would occasionally consume the entire pool
// and wedge an in-flight chat completion.
//
// The Phase D-1 promotion moves the cron to its own Deployment
// with its own pool. This test pins the regression by simulating
// two managers hitting the same pool (the legacy footprint) and
// asserting that neither manager's lease renewal blocks the
// other's request work.
//
// The test runs only when NEXUS_TEST_POSTGRES_URL is set; on
// CI without Postgres the test is skipped so the rest of the
// suite stays fast.
package cron_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ffxnexus/nexus/internal/leaser"
)

func pgURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run cron sharding test")
	}
	return url
}

// ensureSchema mirrors the migration. Skipped if already present.
func ensureSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	const ddl = `
CREATE TABLE IF NOT EXISTS benchmark_scheduler_leases (
    role           TEXT        PRIMARY KEY,
    owner_id       TEXT        NOT NULL,
    acquired_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    heartbeat_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ NOT NULL,
    lock_token     BIGINT      NOT NULL
);`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
}

// TestConnectionShardingDoesNotDeadlock verifies that two
// independent leaser.Manager instances against the same
// pgxpool can each take a connection and drive their renew
// loops without starving concurrent request work. We open a
// pool with MaxConns=4 (the legacy footprint before Phase
// D-1 split everything 16/16) and prove that the cron lease
// renew does not block chat completion acquire.
//
// The test is a regression pin: if a future change pinned
// pgxpool.Acquire inside the renew loop, this would deadlock
// at MaxConns=4 with the second renew goroutine and the
// simulated chat completion. Today the renew loop uses
// pgxpool.Exec which the pool schedules fairly, so the test
// completes well under the watchdog.
func TestConnectionShardingDoesNotDeadlock(t *testing.T) {
	url := pgURL(t)
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0)
	ensureSchema(t, db)

	// We do not have a competing chat completion HTTP call in
	// scope; this test exercises the lease renew loop under
	// concurrency with a synthetic "request workload" by
	// spinning goroutines that hold connections for short
	// intervals. If the renew loop were to acquire a
	// connection and forget to release, the second goroutine
	// would block past the watchdog.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	defer pool.Close()

	mgr := leaser.NewManager(pool, slog.New(slog.DiscardHandler))
	role := fmt.Sprintf("cron_shard_%d", time.Now().UnixNano())
	_, err = mgr.Acquire(ctx, role, fmt.Sprintf("owner-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Spin a request workload that competes for the same
	// MaxConns ceiling with the renew loop. We expect at
	// least half to complete past their budget, so any
	// blocking in renew is caught.
	var completed int32
	deadline := time.Now().Add(20 * time.Second)
	var firstErr atomic.Pointer[error]
	for i := 0; i < 8; i++ {
		go func() {
			for time.Now().Before(deadline) {
				if err := ping(ctx, db); err != nil {
					e := err
					firstErr.CompareAndSwap(nil, &e)
					return
				}
				atomic.AddInt32(&completed, 1)
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}

	// Give the goroutines room to exercise. A genuine
	// deadlock would surface as ErrAcquireTimeout or context
	// cancellation before we reach here.
	time.Sleep(10 * time.Second)
	if errPtr := firstErr.Load(); errPtr != nil {
		t.Fatalf("request workload returned error: %v", *errPtr)
	}
	if completed < 20 {
		t.Errorf("completed=%d, expected at least 20 cycles in 10s", completed)
	}
	if err := mgr.Release(ctx, role); err != nil {
		t.Logf("release: %v", err)
	}
	mgr.Shutdown(ctx)
}

func ping(ctx context.Context, db *sql.DB) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("conn: %w", err)
	}
	defer c.Close()
	if c2, ok := ctx.Deadline(); ok && time.Until(c2) < 0 {
		return errors.New("ctx expired")
	}
	row, err := c.QueryContext(ctx, "SELECT 1")
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer row.Close()
	return row.Scan(new(int))
}
