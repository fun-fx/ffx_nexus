package migrate_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/migrate"
)

// Migration 015 gives eval_scores an org_id. Adding the column is the easy half;
// deciding who owns the rows that were already there is the half that can leak
// data, and it is why these tests exist.
//
// The rule under test: "attribute to the default org" is a correct statement
// about a single-org installation and a guess about any other. A guess here
// means one department's admin opening the Eval page and reading judge
// rationales written about another department's prompts, which is precisely the
// exposure the column was added to close — so the migration must not create it
// while closing it.
//
// These need a real Postgres. The branch under test is a PL/pgSQL DO block
// reading `organizations`; a fake executor would only prove the string was sent.

// runToLatest applies every Postgres migration, which is how a customer upgrade
// reaches 015.
func runToLatest(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Run(context.Background(),
		migrate.NewPostgres(pool, "test"), migs, quietOpts()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// runThrough applies migrations up to and including the given ordinal prefix,
// so a test can build the pre-015 world and then upgrade into it.
func runThrough(t *testing.T, pool *pgxpool.Pool, lastID string) {
	t.Helper()
	all, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	var upto []migrate.Migration
	for _, m := range all {
		upto = append(upto, m)
		if m.ID == lastID {
			break
		}
	}
	if len(upto) == 0 || upto[len(upto)-1].ID != lastID {
		t.Fatalf("migration %q not found in the embedded set", lastID)
	}
	if _, err := migrate.Run(context.Background(),
		migrate.NewPostgres(pool, "test"), upto, quietOpts()); err != nil {
		t.Fatalf("migrate through %s: %v", lastID, err)
	}
}

// seedPreOrgScores writes eval_scores rows the way the pre-015 binary did:
// naming its columns and omitting org_id entirely. That is also exactly what a
// rolled-back application does against the new schema, so this doubles as the
// N+1-schema/N-app write test.
func seedPreOrgScores(t *testing.T, pool *pgxpool.Pool, rows []struct{ TraceID, UserID string }) {
	t.Helper()
	for _, r := range rows {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO eval_scores
			  (trace_id, timestamp, evaluator, metric, score, passed, rationale, judge_model, user_id)
			VALUES ($1, now(), 'slm_judge', 'quality', 0.9, TRUE, 'looks fine', 'gpt-4o-mini', $2)`,
			r.TraceID, r.UserID)
		if err != nil {
			t.Fatalf("seed score %s: %v", r.TraceID, err)
		}
	}
}

func orgOf(t *testing.T, pool *pgxpool.Pool, traceID string) string {
	t.Helper()
	var org string
	if err := pool.QueryRow(context.Background(),
		`SELECT org_id FROM eval_scores WHERE trace_id = $1`, traceID).Scan(&org); err != nil {
		t.Fatalf("read org for %s: %v", traceID, err)
	}
	return org
}

func mkOrg(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO organizations (id, name) VALUES ($1, $1)
		 ON CONFLICT (id) DO NOTHING`, id); err != nil {
		t.Fatalf("create org %s: %v", id, err)
	}
}

func mkUser(t *testing.T, pool *pgxpool.Pool, id, orgID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, org_id, email, password_hash, role)
		 VALUES ($1, $2, $1 || '@example.test', 'x', 'member')`, id, orgID); err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

// TestIntegration015SingleOrgInstallClaimsItsOwnHistory: the common
// self-hosted shape. One org, so every historical score belongs to it — that is
// not an inference, there was nowhere else the traffic could have come from.
func TestIntegration015SingleOrgInstallClaimsItsOwnHistory(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	runThrough(t, pool, "postgres/014_invite_tokens.sql")

	mkOrg(t, pool, "acme")
	mkUser(t, pool, "u-1", "acme")
	seedPreOrgScores(t, pool, []struct{ TraceID, UserID string }{
		{"t-with-user", "u-1"},
		{"t-no-user", ""}, // org-level / legacy traffic
	})

	runToLatest(t, pool)

	for _, tr := range []string{"t-with-user", "t-no-user"} {
		if got := orgOf(t, pool, tr); got != "acme" {
			t.Errorf("%s: single-org install must claim its own history, got org %q", tr, got)
		}
	}
}

// TestIntegration015MultiOrgInstallDoesNotGuess: with more than one org, rows
// that carry a usable user are attributed from it (a real signal), and rows that
// do not are parked in a scope nobody reads rather than handed to whichever org
// happens to be called "default".
func TestIntegration015MultiOrgInstallDoesNotGuess(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	runThrough(t, pool, "postgres/014_invite_tokens.sql")

	mkOrg(t, pool, "acme")
	mkOrg(t, pool, "globex")
	mkUser(t, pool, "u-acme", "acme")
	mkUser(t, pool, "u-globex", "globex")
	seedPreOrgScores(t, pool, []struct{ TraceID, UserID string }{
		{"t-acme", "u-acme"},
		{"t-globex", "u-globex"},
		{"t-orphan", ""},        // no user at all
		{"t-deleted", "u-gone"}, // user has since been removed
	})

	runToLatest(t, pool)

	if got := orgOf(t, pool, "t-acme"); got != "acme" {
		t.Errorf("t-acme: want org acme, got %q", got)
	}
	if got := orgOf(t, pool, "t-globex"); got != "globex" {
		t.Errorf("t-globex: want org globex, got %q", got)
	}
	for _, tr := range []string{"t-orphan", "t-deleted"} {
		got := orgOf(t, pool, tr)
		if got == "acme" || got == "globex" {
			t.Errorf("%s: an unattributable row was guessed into org %q", tr, got)
		}
		if got != "unattributed" {
			t.Errorf("%s: want 'unattributed', got %q", tr, got)
		}
	}
}

// TestIntegration015MultiOrgKeepsRowsOfARealDefaultOrg: "default" is a legal
// org id, and an installation may have one alongside others. Rows belonging to
// its members must stay with it — the parking step keys on "no usable user", not
// on the literal string.
func TestIntegration015MultiOrgKeepsRowsOfARealDefaultOrg(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	runThrough(t, pool, "postgres/014_invite_tokens.sql")

	mkOrg(t, pool, "default")
	mkOrg(t, pool, "acme")
	mkUser(t, pool, "u-def", "default")
	mkUser(t, pool, "u-acme", "acme") // second org in use, so the multi-org branch runs
	seedPreOrgScores(t, pool, []struct{ TraceID, UserID string }{{"t-def", "u-def"}})

	runToLatest(t, pool)

	if got := orgOf(t, pool, "t-def"); got != "default" {
		t.Errorf("a member of a real 'default' org must keep their rows, got %q", got)
	}
}

// TestIntegration015CountsOrgsInUseNotOrgRows pins the distinction that makes
// the single-org branch reachable at all. 001_init.sql unconditionally seeds an
// org called 'default', so `organizations` never holds fewer than one row; a
// customer who created "acme" and moved everyone into it has two org rows but
// one org in use, and withholding their own history from them would be a
// regression dressed up as caution.
func TestIntegration015CountsOrgsInUseNotOrgRows(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	runThrough(t, pool, "postgres/014_invite_tokens.sql")

	// The seeded 'default' org exists but nobody is in it.
	mkOrg(t, pool, "acme")
	mkUser(t, pool, "u-1", "acme")
	seedPreOrgScores(t, pool, []struct{ TraceID, UserID string }{{"t-orphan", ""}})

	var orgRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM organizations`).Scan(&orgRows); err != nil {
		t.Fatal(err)
	}
	if orgRows < 2 {
		t.Fatalf("precondition: expected the seeded default org alongside acme, got %d rows", orgRows)
	}

	runToLatest(t, pool)

	if got := orgOf(t, pool, "t-orphan"); got != "acme" {
		t.Errorf("an empty seeded org must not make this look multi-org; got %q", got)
	}
}

// TestIntegration015IsIdempotent: the migration re-runs on every pod boot via
// the ledger's skip path, but an operator replaying it by hand (or a ledger
// reset) must not reshuffle attributions that later inserts set correctly.
func TestIntegration015IsIdempotent(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	runThrough(t, pool, "postgres/014_invite_tokens.sql")

	mkOrg(t, pool, "acme")
	mkOrg(t, pool, "globex")
	// Two orgs must each have a member, or the installation counts as
	// single-org and legitimately claims the orphan row — which is what an
	// earlier version of this test asserted against by accident.
	mkUser(t, pool, "u-acme", "acme")
	mkUser(t, pool, "u-globex", "globex")
	seedPreOrgScores(t, pool, []struct{ TraceID, UserID string }{
		{"t-acme", "u-acme"},
		{"t-orphan", ""},
	})
	runToLatest(t, pool)

	// A score written after the upgrade, correctly attributed by the new binary.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO eval_scores
		  (trace_id, timestamp, evaluator, metric, score, passed, rationale, judge_model, user_id, org_id)
		VALUES ('t-new', now(), 'slm_judge', 'quality', 0.5, TRUE, '', '', '', 'globex')`); err != nil {
		t.Fatal(err)
	}

	// Replay the migration body directly, which is the worst case: the ledger
	// would normally skip it.
	migs, _ := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	var body string
	for _, m := range migs {
		if m.ID == "postgres/015_eval_scores_org.sql" {
			body = m.SQL
		}
	}
	if body == "" {
		t.Fatal("015 not found in the embedded set")
	}
	if _, err := pool.Exec(context.Background(), body); err != nil {
		t.Fatalf("replay 015: %v", err)
	}

	if got := orgOf(t, pool, "t-acme"); got != "acme" {
		t.Errorf("replay moved an attributed row: t-acme now %q", got)
	}
	if got := orgOf(t, pool, "t-orphan"); got != "unattributed" {
		t.Errorf("replay moved a parked row: t-orphan now %q", got)
	}
	if got := orgOf(t, pool, "t-new"); got != "globex" {
		t.Errorf("replay clobbered a post-upgrade attribution: t-new now %q", got)
	}
}

// TestIntegration015LeavesNoNullOrgAndAcceptsRolledBackWrites covers two
// requirements at once, because they are the same property seen from either end.
//
// Reads filter on org_id, so a NULL would be invisible to every org — the column
// is NOT NULL to make that unrepresentable. And a rolled-back (version N) binary
// omits org_id from its INSERT, so the DEFAULT is what keeps it writing. If the
// column were NOT NULL *without* a default, app rollback would break every eval
// write instead.
func TestIntegration015LeavesNoNullOrgAndAcceptsRolledBackWrites(t *testing.T) {
	pool := freshSchema(t, pgURL(t))
	runToLatest(t, pool)
	ctx := context.Background()

	var nullable string
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'eval_scores' AND column_name = 'org_id'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Errorf("org_id must be NOT NULL so no row can be invisible to every org, got %q", nullable)
	}

	// The version-N write shape: columns named, org_id omitted.
	seedPreOrgScores(t, pool, []struct{ TraceID, UserID string }{{"t-rollback", ""}})
	if got := orgOf(t, pool, "t-rollback"); got != "default" {
		t.Errorf("a rolled-back binary's write must land in the default org, got %q", got)
	}

	var nulls int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM eval_scores WHERE org_id IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 0 {
		t.Errorf("%d rows have a NULL org_id", nulls)
	}
}
