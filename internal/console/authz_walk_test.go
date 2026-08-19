package console

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// walkConsoleRoutes returns "METHOD /path" for every route the console mux
// actually serves, by walking chi's routing tree rather than by reading the
// source. That is the point: a hand-maintained list of routes drifts from the
// code, which is the class of mistake this whole file guards against.
func walkConsoleRoutes(t *testing.T) []string {
	t.Helper()

	mux, ok := newTestServer().Mux().(*chi.Mux)
	if !ok {
		t.Fatal("Mux() no longer returns *chi.Mux; update this walker")
	}
	keys := walkMux(t, mux)
	if len(keys) == 0 {
		t.Fatal("walked zero routes; the walker is broken, not the mux")
	}
	return keys
}

// walkMux is the walk itself, separated from the console server so the
// meta-tests below can point it at a router they built.
func walkMux(t *testing.T, mux *chi.Mux) []string {
	t.Helper()
	var keys []string
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi reports mounted subtrees with a trailing "/*"; normalise so the
		// pattern matches how the route is written at its registration site.
		route = strings.ReplaceAll(route, "/*/", "/")
		keys = append(keys, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return keys
}

// A sanity check on the walker itself, so a silently-broken walker cannot make
// TestEveryRouteHasADeclaredPolicy vacuously pass.
func TestRouteWalkerFindsKnownRoutes(t *testing.T) {
	got := map[string]bool{}
	for _, k := range walkConsoleRoutes(t) {
		got[k] = true
	}
	for _, want := range []string{
		"GET /healthz",
		"GET /api/keys",
		"POST /api/invites",
		"GET /api/me",
	} {
		if !got[want] {
			t.Errorf("walker missed %q; found: %v", want, keysOf(got))
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- meta-tests: does the guard actually bite? ------------------------------
//
// TestEveryRouteHasADeclaredPolicy is only as good as the two pieces underneath
// it: chi.Walk has to see a newly registered route, and undeclaredRoutes has to
// report one it does not recognise. If either quietly stopped working, the guard
// would pass forever while new endpoints shipped unclassified — the failure mode
// is a green build, which is why it needs a negative test of its own rather than
// trust.

// A route registered but not declared must be reported. This is the exact
// scenario the guard exists for: someone adds an endpoint and forgets the table.
func TestUndeclaredRouteIsReported(t *testing.T) {
	walked := append(walkConsoleRoutes(t), "POST /api/exports/all-orgs")

	got := undeclaredRoutes(walked)

	var found bool
	for _, k := range got {
		if k == "POST /api/exports/all-orgs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an undeclared route was not reported; the policy guard is vacuous. reported: %v", got)
	}
	// And nothing else may be reported, or the guard cries wolf and gets muted.
	if len(got) != 1 {
		t.Errorf("guard reported %v; only the undeclared route should be flagged", got)
	}
}

// chi.Walk must see a plain registration. If the walker returned a stale or
// filtered view, the check above would never be handed the new route to reject.
func TestWalkerSeesANewlyRegisteredRoute(t *testing.T) {
	r := chi.NewRouter()
	r.Get("/api/pre-existing", func(http.ResponseWriter, *http.Request) {})

	before := walkMux(t, r)
	if len(before) != 1 {
		t.Fatalf("precondition: want 1 route, got %v", before)
	}

	r.Post("/api/exports/all-orgs", func(http.ResponseWriter, *http.Request) {})
	after := walkMux(t, r)

	var found bool
	for _, k := range after {
		if k == "POST /api/exports/all-orgs" {
			found = true
		}
	}
	if !found {
		t.Errorf("chi.Walk did not report a newly registered route; walked: %v", after)
	}
}

// The skip list in undeclaredRoutes is a real risk: it silences the SPA
// catch-all and the /v1 gateway proxy, and a broader pattern would silence real
// API routes too. This pins that an ordinary console route cannot slip through
// the exclusions.
func TestGuardExclusionsDoNotSwallowConsoleRoutes(t *testing.T) {
	cases := []struct {
		key      string
		reported bool
	}{
		{"POST /api/exports/all-orgs", true}, // ordinary: must be reported
		{"GET /api/admin/secret", true},      // ordinary: must be reported
		{"GET /*", false},                    // SPA asset catch-all
		{"POST /v1/chat/completions", false}, // gateway proxy, virtual-key auth
		{"GET /api/docs/*", false},           // declared subtree
	}
	for _, tc := range cases {
		got := undeclaredRoutes([]string{tc.key})
		if (len(got) > 0) != tc.reported {
			t.Errorf("undeclaredRoutes(%q) reported=%v, want %v", tc.key, len(got) > 0, tc.reported)
		}
	}
}

// The console's only realtime endpoint is the /api/live WebSocket, and it is in
// the policy table as member. There are no console Server-Sent-Events routes:
// the only text/event-stream responses in the product are gateway streaming
// completions, which authenticate with a virtual key and never with a console
// session. This test states that so "does the inventory cover SSE?" has a
// recorded answer rather than an assumption.
func TestRealtimeRoutesAreCoveredByThePolicyTable(t *testing.T) {
	declared := map[string]bool{}
	for _, rp := range consoleRoutePolicies {
		declared[rp.method+" "+rp.path] = true
	}
	if !declared["GET /api/live"] {
		t.Error("the /api/live WebSocket must carry a declared policy like any other route")
	}
	for _, key := range walkConsoleRoutes(t) {
		if strings.Contains(key, "/events") || strings.Contains(key, "/stream") {
			if !declared[key] {
				t.Errorf("a streaming console route %q appeared with no declared policy", key)
			}
		}
	}
}
