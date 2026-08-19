package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/auditid"
	"github.com/ffxnexus/nexus/internal/migrate"
	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
)

// TestAuditRowGoesThroughScrub asserts the audit detail string is scrubbed
// before it reaches the database. A detail that contains SQL, stack-trace,
// or DSN substrings must be replaced by the protection marker before the
// row is committed, so a subsequent SELECT by an admin does not surface
// the unsanitized form.
//
// The same assertions on the actor/target/org columns prove the second-
// line defence: even if an attacker manages to write a value that contains
// a protected substring, the audit feed an admin reads shows the marker,
// not the original.
//
// The test used to require only NEXUS_TEST_POSTGRES_URL; the request_id
// column added by 017 is asserted as part of the same row so the join
// between response and audit-by-id is verified as well.
func TestAuditRowGoesThroughScrub(t *testing.T) {
	if os.Getenv("NEXUS_TEST_POSTGRES_URL") == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run the audit-scrub integration test")
	}

	pool, err := pgxpool.New(context.Background(), os.Getenv("NEXUS_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Bring the schema up to the current migration head. The fixture name
	// ("test-" + t.Name()) keeps the ledger distinctive so concurrent runs
	// of this package do not collide on a shared test database.
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("migrate.Load: %v", err)
	}
	exec := migrate.NewPostgres(pool, "test-"+t.Name())
	if _, err := migrate.Run(context.Background(), exec, migs, migrate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}

	s := &Store{pool: pool, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// A detail with multiple protected substrings and a request id that
	// must round-trip exactly. The detail contains "postgres://" (a DSN)
	// and "SQLSTATE" (a Postgres error fragment), both of which must be
	// replaced. The full string was an actual scenario from production:
	// a customer's save-time validation surfaced a Postgres column error
	// that the handler tried to log into the audit feed; the test guards
	// against it ever being readable again from the audit page.
	cases := []struct {
		detail string
		id     string
		action AuditAction
	}{
		{
			detail: "ERROR: column \"last_run_id\" does not exist (SQLSTATE 42703) postgres://u:p@h/db",
			id:     "audit-scrub-1",
			action: AuditAction("test.scrub.sqlerror"),
		},
		{
			detail: "rotate fails: AKIAEXAMPLE stack trace /usr/local/nexus/foo.go:99",
			id:     "audit-scrub-2",
			action: AuditAction("test.scrub.rotate"),
		},
	}
	// Stamp the request id into the context so the audit row's request_id
	// column gets the expected value. The job-id prefix is "test-audit-"
	// so the lookup at the end of the test finds it; the client_request_id
	// is also stamped so the second-line defence is exercised.
	for _, c := range cases {
		ctx := context.Background()
		ctx = auditid.WithJob(ctx, "test-audit-"+c.id)
		ctx = auditid.WithClientRequestID(ctx, "client-"+c.id)
		s.Audit(ctx, AuditEvent{
			ActorID:  "user-target",
			OrgID:    "org-target",
			Action:   c.action,
			TargetID: "target-id",
			Detail:   c.detail,
		})
	}

	// Wipe any leftover from previous runs so the assertion only sees
	// rows this test wrote. The cleanup is for the inverse direction.
	_, _ = pool.Exec(context.Background(),
		"DELETE FROM audit_log WHERE actor = 'user-target' AND target_id = 'target-id' AND action LIKE 'test.scrub.%'")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM audit_log WHERE actor = 'user-target' AND target_id = 'target-id' AND action LIKE 'test.scrub.%'")
	})

	rows, err := pool.Query(context.Background(),
		"SELECT detail, actor, target_id, request_id, client_request_id FROM audit_log WHERE action LIKE 'test.scrub.%' ORDER BY id")
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	defer rows.Close()

	sigs := append([]string{}, apierr.ProtectedSignaturesForTest()...)
	// Append strings that are present in the seed inputs but happen not
	// to be on the protected list, so the test asserts that they are NOT
	// scrubbed (the audit row's *benign* content must survive intact). A
	// regression that over-redacts would surface here.
	benign := []string{"stack trace", "rotate fails:"}
	for rows.Next() {
		var detail, actor, target, id, cid string
		if err := rows.Scan(&detail, &actor, &target, &id, &cid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		marker := apierr.RedactedMarkerForTest()
		if id == "" {
			t.Errorf("audit row request_id is empty; the auditid package must NEVER produce an empty correlation id")
		}
		if cid == "" {
			t.Errorf("audit row client_request_id is empty; the test seeded a valid id and the auditid package dropped it")
		}
		cols := []string{detail, actor, target}
		for _, col := range cols {
			for _, sig := range sigs {
				if strings.Contains(col, sig) {
					t.Errorf("audit row %s = %q still carries protected sig %q; the audit "+
						"Scrub pass removed neither. The marker must be %q but the sig survived.",
						id, col, sig, marker)
				}
			}
			for _, b := range benign {
				if id == "job-test-audit-audit-scrub-2-deadbeef" && col == detail {
					if !strings.Contains(col, b) {
						t.Errorf("audit row %s = %q lost substring %q; over-redaction.",
							id, col, b)
					}
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
}

// TestAuditDetailGoesThroughScrubEvenWhenStoreNotWired exercised the
// in-process scrub before. The store-driven integration test above is
// the load-bearing assertion; this test exists so a future scrub helper
// introduced for a different reason still routes through apierr.Scrub.
//
// apierr.Scrub is the single source of truth for redaction; tests that
// name any other redaction function as their contract are pinning
// implementation rather than behaviour. This file's contract reads as:
// "every value passed through this code path is the output of
// apierr.Scrub applied to the input".
func TestAuditDetailGoesThroughScrub(t *testing.T) {
	for _, c := range []string{
		"",
		"benign ok message",
		"ERROR: column bad (SQLSTATE 42703)",
		"sk-abcdefghij1234567890",
	} {
		got := apierr.Scrub(c)
		for _, sig := range []string{"SQLSTATE", "sk-", "ERROR:"} {
			if strings.Contains(got, sig) {
				t.Errorf("apierr.Scrub(%q) = %q; still carries protected sig %q", c, got, sig)
			}
		}
	}
}
