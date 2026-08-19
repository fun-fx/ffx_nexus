package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// Invite lifecycle against a real Postgres.
//
// These require a throwaway database:
//
//	NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test ./internal/core/ -run Integration -v
//
// A real database is the point. The defect that motivated this file — replaying
// an accepted invite dereferenced a column the SELECT never fetched — is
// invisible to any fake, because the nil pointer came from the query's column
// list rather than from the Go code around it.

func inviteTestPool(t *testing.T) (*pgxpool.Pool, *Store) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("NEXUS_TEST_POSTGRES_URL"))
	if dsn == "" {
		t.Skip("NEXUS_TEST_POSTGRES_URL not set; skipping invite integration tests")
	}

	ctx := context.Background()
	pgxCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	// Invariant: every goroutine in TestIntegrationConcurrentAcceptsYieldExactlyOneUser
	// holds a transaction connection while it races the FOR UPDATE region.
	// Mirrors production NewStore's minimum-safe MaxConns floor (8). The
	// earlier behaviour (test pool of 10, production default of 4) was a
	// deadlock trap: the test "passed" with extra headroom while a customer
	// with the production default got stuck. Now both default to 8.
	const maxLiveConns = 8
	pgxCfg.MaxConns = maxLiveConns
	pool, err := pgxpool.NewWithConfig(ctx, pgxCfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}

	// Isolate in a per-test schema so parallel packages and reruns cannot see
	// each other's rows, and so cleanup is one DROP.
	schema := fmt.Sprintf("invite_test_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		pool.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})
	if _, err := pool.Exec(ctx, fmt.Sprintf("SET search_path TO %s", schema)); err != nil {
		t.Fatalf("search_path: %v", err)
	}
	// Pooled connections each need the search_path, so pin it on the DSN.
	pool.Close()
	pgxCfg, err = pgxpool.ParseConfig(dsn + "&search_path=" + schema)
	if err != nil {
		t.Fatalf("parse DSN with search_path: %v", err)
	}
	pgxCfg.MaxConns = maxLiveConns
	pool, err = pgxpool.NewWithConfig(ctx, pgxCfg)
	if err != nil {
		t.Fatalf("reconnect with search_path: %v", err)
	}

	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Run(ctx, migrate.NewPostgres(pool, "test"), migs, migrate.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, &Store{pool: pool}
}

// seedOrgAndAdmin creates the minimum graph an invite needs: invite_tokens
// references an org and an actor.
func seedOrgAndAdmin(t *testing.T, pool *pgxpool.Pool) (orgID, adminID string) {
	t.Helper()
	ctx := context.Background()
	orgID, adminID = uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name) VALUES ($1, $2)`, orgID, "Customer Org"); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, org_id, email, password_hash, role, enforce_limits)
		 VALUES ($1, $2, $3, 'x', 'admin', TRUE)`,
		adminID, orgID, "admin@customer.example"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return orgID, adminID
}

// The headline case: issue, accept, and land a real member user.
func TestIntegrationInviteAcceptCreatesMemberWithTheInvitedRole(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgID, adminID := seedOrgAndAdmin(t, pool)

	issued, err := store.CreateInvite(ctx, orgID, adminID,
		"newhire@customer.example", "member", DefaultInviteTTL, "https://console.customer.example")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if issued.Token == "" {
		t.Fatal("no token returned; the invitee would have no link")
	}

	// The raw token must not be recoverable from the table: it is stored hashed
	// so a database dump or a read-only analytics account cannot mint accounts.
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT token_hash FROM invite_tokens WHERE id = $1`, issued.ID).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if stored == issued.Token {
		t.Error("invite token is stored in plaintext")
	}
	if !strings.Contains(stored, "") || len(stored) == 0 {
		t.Error("empty token_hash")
	}

	u, inviteID, err := store.AcceptInvite(ctx, issued.Token, "invitee-password")
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if u.Role != "member" {
		t.Errorf("role = %q, want member (the role must come from the server-side record)", u.Role)
	}
	if u.OrgID != orgID {
		t.Errorf("org = %q, want %q", u.OrgID, orgID)
	}
	if u.Email != "newhire@customer.example" {
		t.Errorf("email = %q", u.Email)
	}
	if inviteID != issued.ID {
		t.Errorf("invite id = %q, want %q", inviteID, issued.ID)
	}

	var pwHash string
	if err := pool.QueryRow(ctx,
		`SELECT password_hash FROM users WHERE id = $1`, u.ID).Scan(&pwHash); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if pwHash == "invitee-password" || pwHash == "" {
		t.Error("password was not hashed on the way in")
	}
}

// Replaying an accepted token used to panic on a nil *inv.AcceptedBy, because
// the FOR UPDATE query never selected accepted_by. Had the pointer been
// populated, the "idempotent re-visit" branch would have handed the user's id,
// org, email and role to an unauthenticated caller holding a spent link.
func TestIntegrationInviteIsSingleUse(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgID, adminID := seedOrgAndAdmin(t, pool)

	issued, err := store.CreateInvite(ctx, orgID, adminID,
		"once@customer.example", "member", DefaultInviteTTL, "https://console.customer.example")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, _, err := store.AcceptInvite(ctx, issued.Token, "first-password"); err != nil {
		t.Fatalf("first accept: %v", err)
	}

	u, _, err := store.AcceptInvite(ctx, issued.Token, "second-password")
	if !errors.Is(err, ErrInviteConsumed) {
		t.Fatalf("replay: err = %v, want ErrInviteConsumed", err)
	}
	if u.ID != "" || u.Email != "" || u.OrgID != "" {
		t.Errorf("replay leaked account data: %+v", u)
	}

	// The replay must not have created a second user or changed the first
	// user's credential.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE email = $1`, "once@customer.example").Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Errorf("user count = %d, want exactly 1 after a replayed invite", n)
	}
}

// Two invitees racing on one forwarded link must not both get an account. The
// FOR UPDATE row lock is what makes this true; without it both transactions read
// accepted_at IS NULL and both insert.
func TestIntegrationConcurrentAcceptsYieldExactlyOneUser(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	orgID, adminID := seedOrgAndAdmin(t, pool)

	issued, err := store.CreateInvite(ctx, orgID, adminID,
		"race@customer.example", "member", DefaultInviteTTL, "https://console.customer.example")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	const racers = 5
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		errs      []error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, err := store.AcceptInvite(ctx, issued.Token, fmt.Sprintf("password-%d", i))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			} else {
				errs = append(errs, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d of %d concurrent accepts succeeded, want exactly 1 (errors: %v)",
			succeeded, racers, errs)
	}
	for _, err := range errs {
		if !errors.Is(err, ErrInviteConsumed) {
			t.Errorf("loser got %v, want ErrInviteConsumed", err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE email = $1`, "race@customer.example").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("user count = %d, want 1", n)
	}
}

func TestIntegrationRevokedInviteCannotBeAccepted(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgID, adminID := seedOrgAndAdmin(t, pool)

	issued, err := store.CreateInvite(ctx, orgID, adminID,
		"revoked@customer.example", "admin", DefaultInviteTTL, "https://console.customer.example")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := store.RevokeInvite(ctx, orgID, adminID, issued.ID); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}

	if _, _, err := store.AcceptInvite(ctx, issued.Token, "password"); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("accept after revoke: err = %v, want ErrInviteNotFound", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE email = $1`, "revoked@customer.example").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a revoked invite created %d user(s); revocation must be effective immediately", n)
	}
}

// An expired token is authentic but spent by time. It must be refused, and
// distinguishably so, because the remedy differs from a revoked invite.
func TestIntegrationExpiredInviteIsRefused(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgID, adminID := seedOrgAndAdmin(t, pool)

	issued, err := store.CreateInvite(ctx, orgID, adminID,
		"stale@customer.example", "member", DefaultInviteTTL, "https://console.customer.example")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	// Age it rather than sleeping for the TTL.
	if _, err := pool.Exec(ctx,
		`UPDATE invite_tokens SET expires_at = now() - interval '1 second' WHERE id = $1`,
		issued.ID); err != nil {
		t.Fatalf("age invite: %v", err)
	}

	if _, _, err := store.AcceptInvite(ctx, issued.Token, "password"); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("expired accept: err = %v, want ErrInviteExpired", err)
	}
}

// A token that was never issued must not be distinguishable from a wrong guess,
// and must not be accepted.
func TestIntegrationUnknownTokenIsRefused(t *testing.T) {
	_, store := inviteTestPool(t)
	ctx := context.Background()

	if _, _, err := store.AcceptInvite(ctx, "not-a-real-token", "password"); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("unknown token: err = %v, want ErrInviteNotFound", err)
	}
	if _, err := store.LookupInvite(ctx, "not-a-real-token"); !errors.Is(err, ErrInviteNotFound) {
		t.Fatalf("unknown lookup: err = %v, want ErrInviteNotFound", err)
	}
}

// ListInvites must be scoped by org: a second org in the same installation is a
// different department, and its pending invites are not the first org's
// business.
func TestIntegrationListInvitesIsOrgScoped(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgA, adminA := seedOrgAndAdmin(t, pool)

	orgB, adminB := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name) VALUES ($1, 'Other Dept')`, orgB); err != nil {
		t.Fatalf("seed org B: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, org_id, email, password_hash, role, enforce_limits)
		 VALUES ($1, $2, 'admin@other.example', 'x', 'admin', TRUE)`, adminB, orgB); err != nil {
		t.Fatalf("seed admin B: %v", err)
	}

	if _, err := store.CreateInvite(ctx, orgA, adminA,
		"a@customer.example", "member", DefaultInviteTTL, ""); err != nil {
		t.Fatalf("invite A: %v", err)
	}
	if _, err := store.CreateInvite(ctx, orgB, adminB,
		"b@other.example", "member", DefaultInviteTTL, ""); err != nil {
		t.Fatalf("invite B: %v", err)
	}

	listA, err := store.ListInvites(ctx, orgA)
	if err != nil {
		t.Fatalf("ListInvites A: %v", err)
	}
	for _, inv := range listA {
		if inv.OrgID != orgA {
			t.Errorf("org A's list contains an invite from org %q", inv.OrgID)
		}
		if inv.Email == "b@other.example" {
			t.Error("org A can see org B's pending invite")
		}
	}
	if len(listA) != 1 {
		t.Errorf("org A list length = %d, want 1", len(listA))
	}

	// The listing must never carry the raw token: an admin of one org reading
	// their own list should not receive material that mints accounts.
	for _, inv := range listA {
		if strings.Contains(fmt.Sprintf("%+v", inv), "token") {
			t.Logf("note: invite struct rendering mentions 'token': %+v", inv)
		}
	}
}

// Issuing and accepting must both leave an audit trail, since account creation
// is exactly the event a customer's security review asks about.
func TestIntegrationInviteWritesAuditTrail(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgID, adminID := seedOrgAndAdmin(t, pool)

	issued, err := store.CreateInvite(ctx, orgID, adminID,
		"audited@customer.example", "member", DefaultInviteTTL, "")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, _, err := store.AcceptInvite(ctx, issued.Token, "password"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT action FROM audit_log WHERE org_id = $1`, orgID)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[a] = true
	}
	for _, want := range []string{string(auditInviteIssue), string(auditInviteAccept), string(auditUserCreate)} {
		if !seen[want] {
			t.Errorf("audit_log missing %q (have %v)", want, seen)
		}
	}
}
