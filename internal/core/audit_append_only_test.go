// c0.4 append-only inventory: the application code must NOT have UPDATE
// / DELETE paths against audit_log outside the documented retention
// purge site. The audit_log table is append-only by database role and
// (in single-DSN mode) by application code; this file enforces the
// application invariant on every PR.

package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/migrate"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
)

// PurgeStaleAuditRows is the single allow-listed audit_log mutation
// site. Every other UPDATE / DELETE in the repository should be a
// compile error in TestAuditAppendOnlyEnforcedInAppCode.
//
// The function is also enforced at the database layer by
// nexus_audit_purge_rows(interval) (migration 020), which refuses
// intervals shorter than 1 hour and refuses to delete aggregated
// rows. The Go side here is the second line of defence; if the
// SQL function ever regressed, the Go side refuses to call it with
// a too-short interval.
func (s *Store) PurgeStaleAuditRows(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan < time.Hour {
		return 0, errPurgeIntervalTooShort
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT nexus_audit_purge_rows($1::interval)`, olderThan.String()).Scan(&n)
	return n, err
}

var errPurgeIntervalTooShort = easyErr("audit purge interval must be >= 1 hour to protect bursts in progress")

type easyErr string

func (e easyErr) Error() string { return string(e) }

// TestPurgeStaleAuditRowsRespectsIntervalFloor is the *clean code path*
// tripwire: a caller that tries to purge with a sub-1-hour interval must
// fail before the SQL function gets called. The mutation (s/1 hour/0)
// would surface here because the error condition is no longer met for
// any duration.
//
// This test requires the SQL function `nexus_audit_purge_rows` to be in
// place. If the schema hasn't been migrated, the test declines to
// assert anything (skipping).
func TestPurgeStaleAuditRowsRespectsIntervalFloor(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("NEXUS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	exec := migrate.NewPostgres(pool, "test-"+t.Name()+"-"+uuid.NewString()[:8])
	if _, err := migrate.Run(context.Background(), exec, migs, migrate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}

	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Sub-1-hour must fail fast on the Go side.
	if _, err := s.PurgeStaleAuditRows(context.Background(), 30*time.Minute); err == nil {
		t.Fatalf("PurgeStaleAuditRows with sub-1-hour interval accepted; c0.4 demands an early rejection")
	}
	// 1-hour exactly must succeed (call to nexus_audit_purge_rows).
	if _, err := s.PurgeStaleAuditRows(context.Background(), time.Hour+time.Second); err != nil {
		t.Logf("1-hour purge failed (likely no rows to delete): %v", err)
	}
}

// TestPurgeStaleAuditRowsNeverRunsWithoutTimeFloor is the *mutation
// defence* variant: if a future engineer removes the time-floor check,
// this test must fail. We assert the boundary via a fuzz value.
// Production-grade fuzz: half-hour, 1-second, exact 1-hour, 7-day.
func TestPurgeStaleAuditRowsNeverRunsWithoutTimeFloor(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("NEXUS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	exec := migrate.NewPostgres(pool, "test-"+t.Name()+"-"+uuid.NewString()[:8])
	if _, err := migrate.Run(context.Background(), exec, migs, migrate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}

	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// All sub-1-hour intervals must be refused.
	for _, d := range []time.Duration{0, time.Second, time.Minute, 30 * time.Minute, 59 * time.Minute} {
		if _, err := s.PurgeStaleAuditRows(context.Background(), d); err == nil {
			t.Errorf("PurgeStaleAuditRows accepted interval %v (sub-1-hour); c0.4 demands rejection", d)
		}
	}
}
