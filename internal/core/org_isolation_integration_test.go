package core

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cross-org isolation at the storage layer.
//
// Requires a throwaway Postgres:
//
//	NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test ./internal/core/ -run Integration -v
//
// These have to hit a real database. The defects were missing WHERE clauses:
// RevokeVirtualKey and DeleteCredential accepted an orgID parameter, recorded it
// in the audit row, and then matched on id alone. Reviewing the Go signature
// tells you nothing — the parameter is right there — so only executing the SQL
// against two orgs' rows shows the boundary is not enforced.

// twoOrgs seeds two orgs, each with an admin, in one migrated schema.
func twoOrgs(t *testing.T, pool *pgxpool.Pool) (orgA, adminA, orgB, adminB string) {
	t.Helper()
	ctx := context.Background()
	mk := func(name, email string) (string, string) {
		org, admin := uuid.NewString(), uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO organizations (id, name) VALUES ($1, $2)`, org, name); err != nil {
			t.Fatalf("seed org %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, org_id, email, password_hash, role, enforce_limits)
			 VALUES ($1, $2, $3, 'x', 'admin', TRUE)`, admin, org, email); err != nil {
			t.Fatalf("seed admin %s: %v", email, err)
		}
		return org, admin
	}
	orgA, adminA = mk("Team A", "admin-a@customer.example")
	orgB, adminB = mk("Team B", "admin-b@customer.example")
	return
}

// An admin of one team must not be able to revoke another team's virtual key.
// Revoking a key takes the other team's application offline, so this was a
// cross-boundary denial of service, and the audit row was written under the
// caller's org — so the affected team could not even find out why.
func TestIntegrationRevokeVirtualKeyIsOrgScoped(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgA, adminA, orgB, adminB := twoOrgs(t, pool)

	keyB, _, err := store.CreateVirtualKey(ctx, orgB, adminB, "", "team-b-key", nil, 0, 0, 0)
	if err != nil {
		t.Fatalf("create key in org B: %v", err)
	}

	// Team A's admin, naming team B's key by its UUID.
	err = store.RevokeVirtualKey(ctx, orgA, adminA, keyB.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org revoke returned %v, want ErrNotFound", err)
	}

	var revoked bool
	if err := pool.QueryRow(ctx,
		`SELECT revoked FROM virtual_keys WHERE id = $1`, keyB.ID).Scan(&revoked); err != nil {
		t.Fatalf("read key: %v", err)
	}
	if revoked {
		t.Error("team B's key was revoked by team A's admin")
	}

	// The owner must still be able to revoke it, or the fix broke the feature.
	if err := store.RevokeVirtualKey(ctx, orgB, adminB, keyB.ID); err != nil {
		t.Fatalf("owner revoke failed: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT revoked FROM virtual_keys WHERE id = $1`, keyB.ID).Scan(&revoked); err != nil {
		t.Fatalf("re-read key: %v", err)
	}
	if !revoked {
		t.Error("the owning org could not revoke its own key")
	}
}

// Deleting another team's provider credential is unrecoverable: the plaintext
// secret only ever existed at the provider and in the ciphertext this row held.
// RotateCredential on the neighbouring line already filtered by org_id, which is
// what made this an oversight rather than a design.
func TestIntegrationDeleteCredentialIsOrgScoped(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgA, adminA, orgB, adminB := twoOrgs(t, pool)

	credID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO provider_credentials (id, org_id, name, provider, secret_ciphertext, secret_last4)
		 VALUES ($1, $2, 'team-b-openai', 'openai', 'ciphertext', '1234')`, credID, orgB); err != nil {
		t.Fatalf("seed credential in org B: %v", err)
	}

	err := store.DeleteCredential(ctx, orgA, adminA, credID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org delete returned %v, want ErrNotFound", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_credentials WHERE id = $1`, credID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatal("team B's provider credential was deleted by team A's admin — unrecoverable")
	}

	if err := store.DeleteCredential(ctx, orgB, adminB, credID); err != nil {
		t.Fatalf("owner delete failed: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_credentials WHERE id = $1`, credID).Scan(&n); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if n != 0 {
		t.Error("the owning org could not delete its own credential")
	}
}

// UserInOrg backs the admin spend views, which take a user id straight from the
// URL. It must not confirm membership across orgs.
func TestIntegrationUserInOrgDoesNotLeakAcrossOrgs(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgA, adminA, orgB, adminB := twoOrgs(t, pool)

	if ok, err := store.UserInOrg(ctx, orgA, adminA); err != nil || !ok {
		t.Fatalf("own-org membership: ok=%v err=%v, want true/nil", ok, err)
	}
	if ok, err := store.UserInOrg(ctx, orgA, adminB); err != nil || ok {
		t.Errorf("org A confirmed membership of org B's admin: ok=%v err=%v", ok, err)
	}
	if ok, err := store.UserInOrg(ctx, orgB, adminA); err != nil || ok {
		t.Errorf("org B confirmed membership of org A's admin: ok=%v err=%v", ok, err)
	}
	// A user id that does not exist anywhere must be indistinguishable from one
	// that exists in another org, or the endpoint becomes a membership oracle.
	if ok, err := store.UserInOrg(ctx, orgA, uuid.NewString()); err != nil || ok {
		t.Errorf("nonexistent user reported as a member: ok=%v err=%v", ok, err)
	}
}

// Listing must not cross orgs either. This passed before the fixes, and is here
// so that a future change to the shared listing queries cannot regress it
// unnoticed.
func TestIntegrationListingsAreOrgScoped(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgA, adminA, orgB, adminB := twoOrgs(t, pool)

	if _, _, err := store.CreateVirtualKey(ctx, orgA, adminA, "", "a-key", nil, 0, 0, 0); err != nil {
		t.Fatalf("key A: %v", err)
	}
	if _, _, err := store.CreateVirtualKey(ctx, orgB, adminB, "", "b-key", nil, 0, 0, 0); err != nil {
		t.Fatalf("key B: %v", err)
	}

	keysA, err := store.ListVirtualKeys(ctx, orgA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	for _, k := range keysA {
		if k.OrgID != orgA {
			t.Errorf("org A's key list contains a key from org %q", k.OrgID)
		}
		if k.Name == "b-key" {
			t.Error("org A can see org B's virtual key")
		}
	}

	usersA, err := store.ListUsers(ctx, orgA)
	if err != nil {
		t.Fatalf("list users A: %v", err)
	}
	for _, u := range usersA {
		if u.OrgID != orgA {
			t.Errorf("org A's user list contains a user from org %q", u.OrgID)
		}
	}

	auditA, err := store.ListAudit(ctx, orgA, AuditListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("list audit A: %v", err)
	}
	for _, a := range auditA {
		if a.OrgID != "" && a.OrgID != orgA {
			t.Errorf("org A's audit log contains an entry from org %q", a.OrgID)
		}
	}
}

// Benchmark history was keyed on the model name alone, so both teams'
// runs came back for any model name a caller could type — including the other
// team's run ids, sample counts and scores. A model name is not a secret and is
// trivially guessable ("gpt-4o"), which is what made this reachable.
//
// This is the same shape of defect as the missing WHERE clauses above and needs
// the same kind of test: the Go signature now has an orgID parameter, and only
// running the SQL against two orgs' rows shows whether it is used.
func TestIntegrationBenchmarkHistoryIsOrgScoped(t *testing.T) {
	pool, store := inviteTestPool(t)
	ctx := context.Background()
	orgA, _, orgB, _ := twoOrgs(t, pool)

	// Both teams benchmark the same model in the same window, with different
	// results — the case where a model-only filter cannot tell them apart.
	seed := func(org, id string, score float64) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO benchmark_runs
			  (id, org_id, model, status, avg_score, min_score, max_score,
			   total_samples, completed_at)
			VALUES ($1, $2, 'gpt-4o', 'completed', $3, $3, $3, 100, now())`,
			id, org, score); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	runA, runB := uuid.NewString(), uuid.NewString()
	seed(orgA, runA, 0.91)
	seed(orgB, runB, 0.42)

	// A pre-attribution row: org_id ''. It belongs to the default org, not to
	// every org, matching console.ownedBenchmark's rule for a single run.
	legacy := uuid.NewString()
	seed("", legacy, 0.77)

	forA, err := store.ListRecentSettledByModel(ctx, orgA, "gpt-4o", 50)
	if err != nil {
		t.Fatalf("history for A: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range forA {
		ids[r.ID] = true
	}
	if !ids[runA] {
		t.Error("org A cannot see its own benchmark run")
	}
	if ids[runB] {
		t.Error("org A can see org B's benchmark run, including its score and run id")
	}
	if ids[legacy] {
		t.Error("an unattributed run was served to a non-default org")
	}

	// The default org inherits the unattributed row.
	forDefault, err := store.ListRecentSettledByModel(ctx, DefaultOrgID, "gpt-4o", 50)
	if err != nil {
		t.Fatalf("history for default org: %v", err)
	}
	var sawLegacy bool
	for _, r := range forDefault {
		if r.ID == legacy {
			sawLegacy = true
		}
		if r.ID == runA || r.ID == runB {
			t.Errorf("the default org saw a run belonging to another org (%s)", r.ID)
		}
	}
	if !sawLegacy {
		t.Error("the default org lost the pre-attribution run")
	}

	// The unfiltered form stays available for the drift watcher and the router's
	// quality snapshot, which are installation-wide by design.
	all, err := store.ListRecentSettledByModel(ctx, "", "gpt-4o", 50)
	if err != nil {
		t.Fatalf("unfiltered history: %v", err)
	}
	if len(all) < 3 {
		t.Errorf("unfiltered history returned %d rows, want all 3", len(all))
	}
}
