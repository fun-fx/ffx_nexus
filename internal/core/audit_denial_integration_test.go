// c0.3 burst-aggregation acceptance: a sequence of denial events for
// the same (action, actor, target) within a 5-minute window MUST
// collapse into a single audit row whose count reflects the burst.
// Outside the window, a new row is written.

package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/auditaggregator"
	"github.com/ffxnexus/nexus/internal/migrate"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
)

// TestAuditDenialCollapsesWithinWindow proves the upsert path is wired
// correctly: two consecutive AuditDenial calls with the same action,
// actor, fingerprint, and first_at increment count instead of writing
// two rows.
//
// Mutation: change AuditDenial to write rows unconditionally — the test
// will then find two rows (count remains 1 on each) rather than one row
// with count > 1.
func TestAuditDenialCollapsesWithinWindow(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("NEXUS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("migrate.Load: %v", err)
	}
	exec := migrate.NewPostgres(pool, "test-"+t.Name()+"-"+uuid.NewString()[:8])
	if _, err := migrate.Run(context.Background(), exec, migs, migrate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}

	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	_, _ = pool.Exec(context.Background(),
		`DELETE FROM audit_log WHERE actor = 'bob' AND action = 'auth.login.denied'`)

	win := auditaggregator.WindowStart(time.Now().UTC())
	fp := auditaggregator.ResourceFingerprint("/v1/chat")

	for i := 0; i < 5; i++ {
		s.AuditDenial(context.Background(), AuditEvent{
			ActorID:  "bob",
			OrgID:    "org-x",
			Action:   AuditAction("auth.login.denied"),
			TargetID: "/v1/chat",
			Detail:   "bad password",
		}, fp, win)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT count, first_at, last_at, resource_fingerprint
		   FROM audit_log
		  WHERE actor = 'bob' AND action = 'auth.login.denied'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var total int
	var firstAt *time.Time
	var lastAt *time.Time
	var fingerprint string
	calls := 0
	for rows.Next() {
		var c int
		var f, l *time.Time
		var fpOut string
		if err := rows.Scan(&c, &f, &l, &fpOut); err != nil {
			t.Fatalf("scan: %v", err)
		}
		calls++
		total += c
		if f != nil {
			firstAt = f
		}
		if l != nil {
			lastAt = l
		}
		fingerprint = fpOut
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 collapsed audit row, got %d (c0.3 collapse path broken)", calls)
	}
	if total != 5 {
		t.Fatalf("collapsed audit row count = %d, want 5 (the upsert should produce this from 5 individual calls)", total)
	}
	if firstAt == nil || lastAt == nil {
		t.Fatalf("first_at / last_at must be set on the collapsed row; got nil")
	}
	if !lastAt.After(*firstAt) || lastAt.Equal(*firstAt) {
		t.Fatalf("last_at %v must be after first_at %v", lastAt, firstAt)
	}
	if fingerprint != fp {
		t.Fatalf("resource_fingerprint = %q, want %q", fingerprint, fp)
	}
}

// TestAuditDenialCreatesNewRowOutsideWindow asserts the burst only
// spans the 5-minute window; a subsequent POST-NOW call writes a
// second row. The 5-minute boundary is a deliberate trade (see
// auditaggregator.WindowStart comment for rationale).
func TestAuditDenialCreatesNewRowOutsideWindow(t *testing.T) {
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
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM audit_log WHERE actor = 'alice' AND action = 'auth.login.denied'`)

	win1 := auditaggregator.WindowStart(time.Now().UTC())
	win2 := win1.Add(auditaggregator.WindowSize + time.Minute) // outside the window
	fp := auditaggregator.ResourceFingerprint("/v1/chat")

	s.AuditDenial(context.Background(), AuditEvent{
		ActorID: "alice", OrgID: "org-x", Action: AuditAction("auth.login.denied"),
		TargetID: "/v1/chat", Detail: "bad",
	}, fp, win1)
	s.AuditDenial(context.Background(), AuditEvent{
		ActorID: "alice", OrgID: "org-x", Action: AuditAction("auth.login.denied"),
		TargetID: "/v1/chat", Detail: "bad",
	}, fp, win2)

	var total int
	rows, _ := pool.Query(context.Background(),
		`SELECT count FROM audit_log WHERE actor = 'alice' AND action = 'auth.login.denied'`)
	defer rows.Close()
	for rows.Next() {
		var c int
		_ = rows.Scan(&c)
		total += c
	}
	if total != 2 {
		t.Fatalf("two windows produced total count = %d, want 2 (no collapse across windows)", total)
	}
}
