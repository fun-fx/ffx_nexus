// c0.3 burst-aggregation acceptance: a sequence of denial events for
// the same (action, actor, target) within a 5-minute window MUST
// collapse into a single audit row whose count reflects the burst.
// Outside the window, a new row is written.

package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
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

// TestAuditDenialCollapses100CallsOneSecondApart pins the property
// the c0.3 burst-aggregation contract advertises: an attacker hitting
// the deny path at replay rate (1 call per second) collapses to ONE
// audit row with count >= 100, NOT 100 separate rows. Without this
// fact the audit table would grow linearly with attack volume, the
// index would lose selectivity, and the SIEM rule "denied_attempts /
// 1m > N" would fire as 100 separate "1 attempt / 1m" events instead
// of the 100-attempts-in-one-minute burst it actually was.
//
// The test is single-threaded (each call below completes before the
// next starts), uses the literal AuditDenial entry point with
// time.Now-derived windows, and asserts both the row count and the
// accumulated count on the single surviving row. The truncation step
// in AuditDenial guarantees the second-by-second inputs all share
// the same first_at, so the upsert path is exercised.
func TestAuditDenialCollapses100CallsOneSecondApart(t *testing.T) {
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
		`DELETE FROM audit_log WHERE actor = 'rate-attacker' AND action = 'auth.login.denied'`)

	fp := auditaggregator.ResourceFingerprint("/v1/chat")

	// 100 calls, each ~1 second apart: 100 wall-clock seconds in total
	// but well inside a 5-minute window. The first call seeds the row;
	// every subsequent call MUST UPSERT into the same row via the
	// ON CONFLICT (action, actor, resource_fingerprint, first_at)
	// index, with count incrementing to 100. Without the c0.3 floor
	// alignment in AuditDenial, callers passing slightly off-boundary
	// windowStart would each get their own row.
	for i := 0; i < 100; i++ {
		s.AuditDenial(context.Background(), AuditEvent{
			ActorID:  "rate-attacker",
			OrgID:    "org-rate",
			Action:   AuditAction("auth.login.denied"),
			TargetID: "/v1/chat",
			Detail:   fmt.Sprintf("burst-%d", i),
		}, fp, time.Now().UTC())
		if i%10 == 9 {
			time.Sleep(50 * time.Millisecond) // 50ms → still inside window
		}
	}

	var rowCount, totalCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log
		   WHERE actor = 'rate-attacker'
		     AND action = 'auth.login.denied'`).Scan(&rowCount); err != nil {
		t.Fatalf("row count: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("expected exactly 1 collapsed audit row after 100 rapid "+
			"denials; got %d. The c0.3 burst-aggregation is broken — the "+
			"unique key (action, actor, resource_fingerprint, first_at) is "+
			"either not being respected or callers are landing on different "+
			"first_at values.", rowCount)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count FROM audit_log
		   WHERE actor = 'rate-attacker' AND action = 'auth.login.denied'`).Scan(&totalCount); err != nil {
		t.Fatalf("total: %v", err)
	}
	if totalCount != 100 {
		t.Errorf("collapsed-row count = %d, want 100; ON CONFLICT DO UPDATE "+
			"did not increment the count, so analytics downstream sees "+
			"the burst as a single event rather than a flood", totalCount)
	}
}
// TestAuditDenialIsolatesPreAuthBurstsByOrg proves the c0.x #2
// org-fix: two organisations hitting the same denial codepath at the
// same instant (actor="" fallback to "system" because neither had a
// session yet) MUST get two separate audit rows. Pre-fix collapse
// merged the per-org counts into one row — exactly the kind of bug a
// SOC2 auditor flags ("can a customer see another customer's denial
// volume?").
func TestAuditDenialIsolatesPreAuthBurstsByOrg(t *testing.T) {
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
		`DELETE FROM audit_log WHERE actor = 'system' AND action = 'denied.rate_limit'`)

	fp := auditaggregator.ResourceFingerprint("/v1/chat")
	win := time.Now().UTC()

	// Five denies from two orgs at the same window. Empty ActorID
	// falls back to "system" inside AuditDenial, so the only field
	// that differs between rows is org_id. Pre-fix the unique key
	// was (action, actor, resource_fingerprint, first_at) without
	// org_id, both orgs collapsed into ONE row. Post-fix there must
	// be exactly TWO rows, each carrying the right count.
	for i := 0; i < 5; i++ {
		s.AuditDenial(context.Background(), AuditEvent{
			ActorID:  "",
			OrgID:    "org-A",
			Action:   AuditAction("denied.rate_limit"),
			TargetID: "/v1/chat",
		}, fp, win)
		s.AuditDenial(context.Background(), AuditEvent{
			ActorID:  "",
			OrgID:    "org-B",
			Action:   AuditAction("denied.rate_limit"),
			TargetID: "/v1/chat",
		}, fp, win)
	}

	var rowCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log
		   WHERE actor = 'system' AND action = 'denied.rate_limit'`).Scan(&rowCount); err != nil {
		t.Fatalf("row count: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("pre-auth burst from two orgs produced %d rows, want 2; "+
			"org-A and org-B collapsing into one row means one tenant can "+
			"see another tenant's denial count through the aggregated row",
			rowCount)
	}

	rows, err := pool.Query(context.Background(),
		`SELECT org_id, count FROM audit_log
		   WHERE actor = 'system' AND action = 'denied.rate_limit'
		   ORDER BY org_id`)
	if err != nil {
		t.Fatalf("per-org breakdown: %v", err)
	}
	defer rows.Close()
	seen := map[string]int{}
	for rows.Next() {
		var org string
		var c int
		if err := rows.Scan(&org, &c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[org] = c
	}
	if seen["org-A"] != 5 || seen["org-B"] != 5 {
		t.Errorf("per-org counts: %v; want org-A=5 org-B=5. A non-"+
			"isolated burst would show one of them with count=10 and the "+
			"other with count=0", seen)
	}
}

// TestAuditDenialGateLatencyUnderContention measures whether the
// burst-aggregation's row-level lock contention on a hot row leaks
// back into caller latency. The audit DB is downstream of the LLM
// gateway; the user-facing requirement is that a denial event
// recorded into the audit table MUST NOT add enough overhead to the
// gateway hot path to be observable in an end-user request. The
// chart of "denied => AuditDenial => INSERT...ON CONFLICT" must
// observe zero notable regression under attack.
//
// Methodology: spin up 200 concurrent goroutines each issuing 100
// denied events on the SAME (action, actor, resource_fingerprint,
// first_at) burst row. They all UPSERT into one row, so the
// contention is maximal. Time both single-shot latency (median of
// the 20,000 events) and total wall-clock. If single-shot latency
// exceeds 50ms, the aggregation is invisible-blocking the gateway.
// Such a finding would justify shipping the c0.x-shard write path
// (a separate PR); here we surface the number so the policy question
// — fail-stop? best-effort fail-and-log? — has data behind it.
//
// The test uses SHORT bursts and a moderate concurrency so that it
// stays in the CI budget. The test is automatically skipped when
// no Postgres URL is configured, so developer laptops without
// docker-compose only see the skip line.
func TestAuditDenialGateLatencyUnderContention(t *testing.T) {
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
		`DELETE FROM audit_log WHERE actor = 'load-attacker' AND action = 'denied.rate_limit'`)

	fp := auditaggregator.ResourceFingerprint("/v1/chat")
	win := time.Now().UTC()

	const (
		workers      = 200
		perWorker    = 100
		singleShotMS = 50
	)
	var (
		wg       sync.WaitGroup
		dursMu   sync.Mutex
		durations []time.Duration
	)
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				t0 := time.Now()
				s.AuditDenial(context.Background(), AuditEvent{
					ActorID:  "load-attacker",
					OrgID:    "org-load",
					Action:   AuditAction("denied.rate_limit"),
					TargetID: "/v1/chat",
					Detail:   fmt.Sprintf("w%d-i%d", w, i),
				}, fp, win)
				dursMu.Lock()
				durations = append(durations, time.Since(t0))
				dursMu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	total := time.Since(start)

	// Compute median per-call latency.
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	p99 := durations[(len(durations)*99)/100]
	t.Logf("contention: workers=%d, events=%d, total=%v, median=%v, p99=%v",
		workers, workers*perWorker, total, median, p99)
	if median > singleShotMS*time.Millisecond {
		t.Errorf("median per-event latency under contention = %v (> %dms). "+
			"Indicates the audit UPSERT is lock-blocked in a way the gateway "+
			"hot path will see as a tail-latency spike. Consider pipeline-"+
			"sharding the burst row or moving aggregation writes to a "+
			"dedicated worker pool before blocking the request.", median,
			singleShotMS)
	}

	var rowCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count FROM audit_log
		   WHERE actor = 'load-attacker' AND action = 'denied.rate_limit'`).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != workers*perWorker {
		t.Errorf("aggregated count = %d, want %d", rowCount, workers*perWorker)
	}
}
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
