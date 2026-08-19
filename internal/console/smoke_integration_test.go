package console

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	nexus "github.com/ffxnexus/nexus"
	"github.com/ffxnexus/nexus/internal/benchmark"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/core/crypto"
	"github.com/ffxnexus/nexus/internal/migrate"
	"github.com/ffxnexus/nexus/internal/router"
)

// Day one. A customer installs the chart against an empty database, logs in, and
// clicks through the console.
//
// Every schema defect this repository has shipped presented exactly here and
// nowhere earlier: invite_tokens missing made the invite list 500, a missing
// benchmark_runs column made the leaderboard 500, and benchmark_schedules
// missing last_run_id made the schedule list 500. All three compiled, passed unit
// tests against fakes, and broke on the customer's first click.
//
// So this test does the first click. It walks chi's routing tree — every GET the
// console serves, discovered rather than listed, so a new screen is covered the
// day it is added — and calls each one against a real, freshly migrated, EMPTY
// database with a real admin session.
//
// The assertion is narrow and load-bearing: no 5xx. An empty database is a normal
// state, so a read must answer 200 with an empty collection (or 404 for a
// specific id that does not exist). A 500 means the query could not run, and on an
// empty database the overwhelmingly likely reason is that the schema does not
// match the code.
//
//	NEXUS_TEST_POSTGRES_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
//	  go test ./internal/console/ -run Integration -v

// smokeEnv is a console server wired to a real, migrated, empty database.
type smokeEnv struct {
	srv     *Server
	mux     http.Handler
	store   *core.Store
	session string
	orgID   string
	userID  string
}

func newSmokeEnv(t *testing.T) *smokeEnv {
	t.Helper()
	url := os.Getenv("NEXUS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_POSTGRES_URL to run the empty-install smoke test")
	}
	ctx := context.Background()

	// Each run gets its own schema, so "empty" really is empty and a leftover row
	// from another test cannot make a broken read look healthy.
	schema := "smoke_" + strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			admin.Close()
			t.Fatalf("prepare schema: %v", err)
		}
	}
	admin.Close()
	t.Cleanup(func() {
		if c, err := pgxpool.New(context.Background(), url); err == nil {
			_, _ = c.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
			c.Close()
		}
	})

	scopedURL := url
	if strings.Contains(scopedURL, "?") {
		scopedURL += "&search_path=" + schema
	} else {
		scopedURL += "?search_path=" + schema
	}

	// Migrate exactly as the pre-upgrade Job does: the whole set, in order, with no
	// hand-picked subset. A test that lists the migrations it wants is how a new
	// migration gets left out of test coverage.
	pool, err := pgxpool.New(ctx, scopedURL)
	if err != nil {
		t.Fatalf("scoped pool: %v", err)
	}
	migs, err := migrate.Load(nexus.Migrations, migrate.EnginePostgres)
	if err != nil {
		pool.Close()
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Run(ctx, migrate.NewPostgres(pool, "smoke"), migs, migrate.Options{}); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	pool.Close()

	cipher, err := crypto.NewCipher(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	store, err := core.NewStore(ctx, scopedURL, cipher)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(store.Close)

	// A real org row: users.org_id is a foreign key, so a fabricated org id would
	// fail here rather than in the reads under test.
	orgID := uuid.NewString()
	seedPool, err := pgxpool.New(ctx, scopedURL)
	if err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	if _, err := seedPool.Exec(ctx,
		`INSERT INTO organizations (id, name) VALUES ($1, $2)`, orgID, "Smoke Customer"); err != nil {
		seedPool.Close()
		t.Fatalf("seed org: %v", err)
	}
	seedPool.Close()

	user, err := store.CreateUser(ctx, orgID, "", "admin@smoke.example", "correct-horse-battery", core.RoleAdmin)
	if err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	token, err := store.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	// reader is nil: ClickHouse is optional, and an installation without it must
	// degrade rather than 500. That is part of what this test checks.
	srv := NewServer(nil, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// The DB-backed subsystems must be wired, or the endpoints that carried all
	// three shipped schema defects answer 503 "not configured" without ever
	// reaching SQL — and this test would pass while proving nothing about them.
	//
	// Only genuinely optional, non-database subsystems are left unwired, and
	// TestIntegrationTheDayOneScreensAnswerOnAnEmptyInstall pins which those are.
	// The runner's store is real; its provider token source is a stub. Nexus does
	// not hold a benchmark-provider token on a fresh install, and the rule for this
	// work is not to call a paid vendor API, so the stub returns none — the runner
	// then reports "no token" rather than reaching the network. Every schedule and
	// run read still goes to the real database, which is the path under test.
	srv.SetBenchmarks(benchmark.NewRunner(store, nil, noProviderToken{}, "", slog.New(slog.DiscardHandler)))
	srv.SetQualityRouter(emptyQualityRouter{})
	return &smokeEnv{
		srv:     srv,
		mux:     srv.Mux(),
		store:   store,
		session: token,
		orgID:   orgID,
		userID:  user.ID,
	}
}

// noProviderToken stands in for the benchmark provider credential. A fresh
// install has none, and this work must not call a paid vendor API.
type noProviderToken struct{}

func (noProviderToken) Token(context.Context, string) (string, error)  { return "", nil }
func (noProviderToken) TeamID(context.Context, string) (string, error) { return "", nil }

// emptyQualityRouter is the router with no models scored yet — the state of every
// installation on day one. The leaderboard must render that, not 503.
type emptyQualityRouter struct{}

func (emptyQualityRouter) BlendConfig() (router.CombinedWeights, time.Duration, router.BenchmarkScoreSource) {
	return router.CombinedWeights{}, 0, nil
}
func (emptyQualityRouter) KnownModels(context.Context) []string { return nil }

func (e *smokeEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.session})
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// samplePathParams fills chi path parameters with ids that do not exist. A read
// for a missing id must answer 404, not 500 — and reaching the "does it exist"
// check means the query ran, which is precisely what is being verified.
var samplePathParams = map[string]string{
	"id":         "00000000-0000-0000-0000-000000000000",
	"traceID":    "00000000000000000000000000000000",
	"trace_id":   "00000000000000000000000000000000",
	"runID":      "00000000-0000-0000-0000-000000000000",
	"scheduleID": "00000000-0000-0000-0000-000000000000",
	"name":       "does-not-exist",
	"provider":   "openai",
	"model":      "gpt-4o",
	"key":        "does-not-exist",
	"token":      "does-not-exist",
	"userID":     "00000000-0000-0000-0000-000000000000",
	"pluginID":   "does-not-exist",
	"profileID":  "does-not-exist",
	"sessionID":  "00000000-0000-0000-0000-000000000000",
	"day":        "2026-01-01",
	"turnID":     "00000000-0000-0000-0000-000000000000",
}

// concretise turns a chi pattern into a requestable path.
func concretise(pattern string) (string, bool) {
	if strings.Contains(pattern, "*") {
		// Wildcard subtrees are the SPA handler and the gateway proxy, neither of
		// which reads the control-plane schema.
		return "", false
	}
	out := make([]string, 0, 8)
	for _, seg := range strings.Split(strings.Trim(pattern, "/"), "/") {
		if !strings.HasPrefix(seg, "{") {
			out = append(out, seg)
			continue
		}
		name := strings.Trim(seg, "{}")
		if i := strings.Index(name, ":"); i >= 0 {
			name = name[:i]
		}
		val, ok := samplePathParams[name]
		if !ok {
			return "", false
		}
		out = append(out, val)
	}
	return "/" + strings.Join(out, "/"), true
}

// unconfiguredSubsystems records the routes that answer 503 in this harness, and
// why wiring them is not possible or not useful here.
//
// Every entry is a route this test does NOT cover, so the list is the honest
// statement of the gap. An unlisted 503 fails, because a route that refuses before
// reaching SQL is invisible to a schema check while looking like a pass — which is
// the same shape as the -run filter that hid the original defect.
var unconfiguredSubsystems = map[string]string{
	"/api/eval/profiles/": "profiles have no Postgres table; the store is in-memory " +
		"and config-seeded, so there is no schema to contract-check",
	"/api/eval/plugins/": "the console's plugin source is an adapter assembled in " +
		"package main. The underlying eval_plugins SQL is covered by " +
		"internal/schemacontract, which prepares every statement against the migrated schema",
	"/api/eval/config": "reports the eval worker's runtime config; there is no worker " +
		"in this harness and the route reads no table",
	"/api/eval/benchmarks/credential": "reads the benchmark provider credential from " +
		"the encrypted credential store, which a fresh install does not have",
}

// unconfiguredSeen tracks which entries actually fired, so a stale entry — a route
// that has since been wired, or removed — is reported rather than silently
// excusing nothing.
var unconfiguredSeen = map[string]bool{}

// The headline test.
func TestIntegrationEveryConsoleReadSurvivesAnEmptyInstall(t *testing.T) {
	env := newSmokeEnv(t)
	for k := range unconfiguredSeen {
		delete(unconfiguredSeen, k)
	}

	mux, ok := env.mux.(*chi.Mux)
	if !ok {
		t.Fatal("Mux() no longer returns *chi.Mux; update this smoke test")
	}

	var gets []string
	if err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet {
			gets = append(gets, strings.ReplaceAll(route, "/*/", "/"))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	if len(gets) == 0 {
		t.Fatal("walked zero GET routes; the walker is broken and this test proves nothing")
	}
	sort.Strings(gets)

	called, skipped := 0, 0
	var unparameterised []string
	for _, pattern := range gets {
		path, ok := concretise(pattern)
		if !ok {
			skipped++
			unparameterised = append(unparameterised, pattern)
			continue
		}
		rec := env.get(t, path)
		called++

		if rec.Code == http.StatusServiceUnavailable {
			if reason, ok := unconfiguredSubsystems[pattern]; ok {
				t.Logf("  503 by design: %s — %s", pattern, reason)
				unconfiguredSeen[pattern] = true
				continue
			}
			t.Errorf("GET %s (as %s) returned 503 on an empty, freshly migrated "+
				"database, and no reason is recorded:\n  body: %s\n"+
				"  A 503 means the handler refused before reaching SQL, so this route is "+
				"NOT covered by this test — exactly the blind spot that let three schema "+
				"defects ship. Either wire the subsystem in newSmokeEnv so the read "+
				"reaches the database, or record why it cannot be in "+
				"unconfiguredSubsystems.", path, pattern, truncate(rec.Body.String(), 300))
			continue
		}

		if rec.Code >= 500 {
			t.Errorf("GET %s (as %s) returned %d on an empty, freshly migrated database\n"+
				"  body: %s\n"+
				"  An empty database is a normal state: this read should answer 200 with an "+
				"empty collection, or 404 for an id that does not exist. A 5xx here is what "+
				"a customer sees on their first click, and every schema defect this "+
				"repository has shipped looked exactly like this.",
				path, pattern, rec.Code, truncate(rec.Body.String(), 400))
		}
	}

	t.Logf("called %d of %d GET routes; %d could not be given concrete path parameters", called, len(gets), skipped)
	for _, p := range unparameterised {
		t.Logf("  not called: %s", p)
	}

	for pattern, reason := range unconfiguredSubsystems {
		if !unconfiguredSeen[pattern] {
			t.Errorf("unconfiguredSubsystems excuses %s (%s), but that route did not "+
				"answer 503. Either it is now wired — remove the entry so the route is "+
				"checked — or it no longer exists.", pattern, reason)
		}
	}

	// A drifting samplePathParams map would silently shrink coverage until the test
	// called almost nothing while still passing.
	if called < len(gets)*2/3 {
		t.Errorf("only %d of %d GET routes were callable. Add the missing path "+
			"parameter names to samplePathParams; below two thirds this test stops "+
			"being a meaningful check of a fresh install.", called, len(gets))
	}
}

// The screens the customer actually opens, named explicitly.
//
// The walk above is exhaustive, but exhaustive-by-discovery has a failure mode: if
// a route is renamed or unregistered, the walk simply stops covering it and still
// passes. Naming the day-one screens means removing one fails the build.
func TestIntegrationTheDayOneScreensAnswerOnAnEmptyInstall(t *testing.T) {
	env := newSmokeEnv(t)

	// Every one of these is a screen a customer opens in their first session, and
	// the three shipped schema defects were on this list.
	screens := []struct {
		screen string
		path   string
	}{
		{"who am I", "/api/me"},
		{"virtual keys", "/api/keys"},
		{"invites", "/api/invites"},
		{"users", "/api/users"},
		{"provider credentials", "/api/credentials"},
		{"benchmark runs", "/api/eval/benchmarks"},
		{"benchmark schedules", "/api/eval/benchmarks/schedules"},
		{"benchmark leaderboard", "/api/eval/benchmarks/leaderboard"},
		{"spend and costs", "/api/me/spend/summary"},
		{"daily cost breakdown", "/api/me/spend/daily"},
		{"audit log", "/api/audit"},
	}

	for _, s := range screens {
		rec := env.get(t, s.path)
		switch {
		case rec.Code >= 500:
			t.Errorf("the %s screen (GET %s) returned %d on an empty install:\n  %s",
				s.screen, s.path, rec.Code, truncate(rec.Body.String(), 400))
		case rec.Code == http.StatusNotFound:
			t.Errorf("the %s screen (GET %s) is not registered. Either the route was "+
				"renamed — update this list — or the screen has no backend and the console "+
				"shows an error.", s.screen, s.path)
		case rec.Code == http.StatusOK:
			// An empty install must return an empty collection, not null, or the
			// console renders "cannot read property length of null".
			if body := strings.TrimSpace(rec.Body.String()); body == "null" {
				t.Errorf("the %s screen (GET %s) returned bare null rather than an empty "+
					"collection; the console cannot render that", s.screen, s.path)
			}
		}
	}
}

// A read that fails because ClickHouse is absent must say so, not 500. ClickHouse
// is optional in the chart, so this is a supported configuration and not an error
// state — the trace and cost screens should degrade to an explicit "not
// configured" rather than looking like a broken installation.
func TestIntegrationClickHouseBackedScreensDegradeWithoutClickHouse(t *testing.T) {
	env := newSmokeEnv(t) // constructed with a nil reader

	for _, path := range []string{
		"/api/traces",
		"/api/observability/summary",
	} {
		rec := env.get(t, path)
		if rec.Code == http.StatusNotFound {
			continue // route does not exist under this name; the walk above covers it
		}
		if rec.Code >= 500 {
			t.Errorf("GET %s returned %d with ClickHouse absent:\n  %s\n"+
				"ClickHouse is optional in the chart, so this is a supported "+
				"configuration. The screen should report that traces are not configured, "+
				"not present as a server fault.",
				path, rec.Code, truncate(rec.Body.String(), 300))
		}
	}
}

// Non-vacuity for the whole file: the session must really authenticate, or every
// request above is a 401 that trivially is not a 5xx.
func TestIntegrationTheSmokeSessionIsGenuinelyAuthenticated(t *testing.T) {
	env := newSmokeEnv(t)

	rec := env.get(t, "/api/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me with the smoke session returned %d (%s). Every other "+
			"assertion in this file is then meaningless, because an unauthenticated "+
			"request never reaches a database query.", rec.Code, rec.Body.String())
	}

	var me map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /api/me: %v (%s)", err, rec.Body.String())
	}
	if email, _ := me["email"].(string); email != "admin@smoke.example" {
		t.Errorf("/api/me reports email %q, want the bootstrapped admin", email)
	}

	// And without the cookie the same route must refuse, proving the cookie is
	// what authenticated the requests rather than an absent guard.
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	anon := httptest.NewRecorder()
	env.mux.ServeHTTP(anon, req)
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/me without a session returned %d, want 401", anon.Code)
	}
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + " …"
	}
	return s
}
