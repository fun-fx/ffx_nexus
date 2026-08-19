package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// routePolicy is the authorization contract for one console route.
//
// This table is the product decision, written down. Every route the console
// exposes must appear here with an explicit policy, and the test below walks the
// table against the real mux. Adding a route without classifying it fails
// TestEveryRouteHasADeclaredPolicy, so "I forgot to add requireAdmin" becomes a
// red build instead of an unauthenticated endpoint.
//
// It exists because two org-wide read endpoints — GET /api/keys and
// GET /api/credentials — were registered with no guard at all while their POST
// and DELETE siblings on the same line were admin-only. Nothing in review caught
// it: the routes look right unless you compare them character by character.
type routePolicy struct {
	method string
	path   string

	// policy is one of:
	//   public - deliberately reachable with no session
	//   member - any authenticated user
	//   admin  - authenticated with role=admin
	policy string

	// why documents public routes. Required for them, so that making a route
	// public is a deliberate, argued act.
	why string
}

// conditionalRoutes are registered only when an optional dependency is wired at
// boot (a plugin tester, a per-vendor key resolver). A bare test server does not
// wire them, so their absence is expected - but when present they must still
// obey the policy declared above.
var conditionalRoutes = map[string]bool{
	"POST /api/eval/plugins/{name}/webhook": true,
	"POST /api/eval/plugins/{name}/test":    true,
	"POST /api/eval/plugins/{name}/fire":    true,
	"GET /api/eval/plugins/{name}/keys":     true,
	"PUT /api/eval/plugins/{name}/keys":     true,
	"DELETE /api/eval/plugins/{name}/keys":  true,
}

// consoleRoutePolicies covers every route registered by Server.Mux().
var consoleRoutePolicies = []routePolicy{
	// --- public: unauthenticated by design -----------------------------------
	{"GET", "/healthz", "public", "kubelet liveness; must not depend on the database"},
	{"GET", "/readyz", "public", "kubelet readiness; probes cannot carry a session"},
	{"GET", "/api/auth/config", "public", "the login page must know whether SSO and signup are on before anyone can log in"},
	{"POST", "/api/auth/login", "public", "this is how a session is obtained"},
	{"POST", "/api/auth/register", "public", "self-signup, itself gated by NEXUS_ALLOW_SIGNUP"},
	{"POST", "/api/auth/logout", "public", "clearing a cookie must work even once the session is already invalid"},
	{"GET", "/api/auth/sso/login", "public", "start of the OIDC redirect dance"},
	{"GET", "/api/auth/sso/callback", "public", "the IdP redirects the browser here with no Nexus session yet"},
	{"GET", "/api/invite/{token}", "public", "the invitee has no account yet; the 256-bit token is the credential"},
	{"POST", "/api/invite/{token}/accept", "public", "same: the invite token authenticates this call"},
	{"POST", "/api/eval/plugins/{name}/webhook", "public", "external eval vendors push scores here; authenticated by a per-plugin signing secret, not a session"},

	// --- member --------------------------------------------------------------
	{"GET", "/api/traces", "member", ""},
	{"GET", "/api/turns", "member", ""},
	{"GET", "/api/stats", "member", ""},
	{"GET", "/api/stats/providers", "member", ""},
	{"GET", "/api/live", "member", ""},
	{"GET", "/api/routing", "member", ""},
	{"GET", "/api/evals", "member", ""},
	{"GET", "/api/ui/observability", "member", ""},
	{"GET", "/api/me", "member", ""},
	{"PATCH", "/api/me", "member", ""},
	{"GET", "/api/me/stats", "member", ""},
	{"GET", "/api/me/traces", "member", ""},
	{"GET", "/api/me/turns", "member", ""},
	{"GET", "/api/me/quality", "member", ""},
	{"GET", "/api/me/spend/summary", "member", ""},
	{"GET", "/api/me/spend/daily", "member", ""},
	{"GET", "/api/me/spend/daily/{day}/breakdown", "member", ""},
	{"GET", "/api/me/keys", "member", ""},
	{"POST", "/api/me/keys", "member", ""},
	{"DELETE", "/api/me/keys/{id}", "member", ""},
	{"GET", "/api/me/credentials", "member", ""},
	{"POST", "/api/me/credentials/preflight", "member", ""},
	{"POST", "/api/me/credentials", "member", ""},
	{"POST", "/api/me/credentials/{id}/rotate", "member", ""},
	{"DELETE", "/api/me/credentials/{id}", "member", ""},
	{"GET", "/api/me/playground/catalog", "member", ""},
	{"GET", "/api/eval/profiles/", "member", ""},
	{"PATCH", "/api/eval/profiles/{id}", "member", ""},
	{"DELETE", "/api/eval/profiles/{id}", "member", ""},
	// The docs tree is declared as a subtree below, because it is mounted as a
	// whole router behind one guard rather than route by route.

	// --- admin ---------------------------------------------------------------
	{"GET", "/api/keys", "admin", ""},
	{"POST", "/api/keys", "admin", ""},
	{"DELETE", "/api/keys/{id}", "admin", ""},
	{"GET", "/api/credentials", "admin", ""},
	{"POST", "/api/credentials", "admin", ""},
	{"POST", "/api/credentials/{id}/rotate", "admin", ""},
	{"DELETE", "/api/credentials/{id}", "admin", ""},
	{"GET", "/api/eval/config", "admin", ""},
	{"PATCH", "/api/eval/config", "admin", ""},
	{"POST", "/api/eval/profiles/", "admin", ""},
	{"GET", "/api/eval/plugins/", "admin", ""},
	{"POST", "/api/eval/plugins/", "admin", ""},
	{"PATCH", "/api/eval/plugins/{id}", "admin", ""},
	{"DELETE", "/api/eval/plugins/{id}", "admin", ""},
	{"POST", "/api/eval/plugins/{name}/test", "admin", ""},
	{"POST", "/api/eval/plugins/{name}/fire", "admin", ""},
	{"POST", "/api/eval/plugins/{name}/automation", "admin", ""},
	{"GET", "/api/eval/plugins/{name}/keys", "admin", ""},
	{"PUT", "/api/eval/plugins/{name}/keys", "admin", ""},
	{"DELETE", "/api/eval/plugins/{name}/keys", "admin", ""},
	{"GET", "/api/eval/benchmarks/", "admin", ""},
	{"POST", "/api/eval/benchmarks/", "admin", ""},
	{"GET", "/api/eval/benchmarks/schedules/", "admin", ""},
	{"POST", "/api/eval/benchmarks/schedules/", "admin", ""},
	{"GET", "/api/eval/benchmarks/schedules/{id}", "admin", ""},
	{"DELETE", "/api/eval/benchmarks/schedules/{id}", "admin", ""},
	{"POST", "/api/eval/benchmarks/schedules/{id}/pause", "admin", ""},
	{"POST", "/api/eval/benchmarks/schedules/{id}/resume", "admin", ""},
	{"POST", "/api/eval/benchmarks/validate", "admin", ""},
	{"POST", "/api/eval/benchmarks/push-report", "admin", ""},
	{"GET", "/api/eval/benchmarks/push-report", "admin", ""},
	{"GET", "/api/eval/benchmarks/models", "admin", ""},
	{"POST", "/api/eval/benchmarks/refresh", "admin", ""},
	{"GET", "/api/eval/benchmarks/credential", "admin", ""},
	{"PUT", "/api/eval/benchmarks/credential", "admin", ""},
	{"DELETE", "/api/eval/benchmarks/credential", "admin", ""},
	{"GET", "/api/eval/benchmarks/quality", "admin", ""},
	{"POST", "/api/eval/benchmarks/quality", "admin", ""},
	{"GET", "/api/eval/benchmarks/leaderboard", "admin", ""},
	{"GET", "/api/eval/benchmarks/{model}/history", "admin", ""},
	{"GET", "/api/eval/benchmarks/{id}", "admin", ""},
	{"DELETE", "/api/eval/benchmarks/{id}", "admin", ""},
	{"POST", "/api/eval/benchmarks/{id}/cancel", "admin", ""},
	{"GET", "/api/eval/benchmarks/{id}/logs", "admin", ""},
	{"GET", "/api/users", "admin", ""},
	{"POST", "/api/users", "admin", ""},
	{"DELETE", "/api/users/{id}", "admin", ""},
	{"GET", "/api/users/quality", "admin", ""},
	{"GET", "/api/users/{id}/spend/summary", "admin", ""},
	{"GET", "/api/users/{id}/spend/daily", "admin", ""},
	{"GET", "/api/users/{id}/spend/daily/{day}/breakdown", "admin", ""},
	{"GET", "/api/audit", "admin", ""},
	{"GET", "/api/invites", "admin", ""},
	{"POST", "/api/invites", "admin", ""},
	{"DELETE", "/api/invites/{id}", "admin", ""},
}

// declaredSubtrees are whole routers mounted behind a single guard. chi reports
// such a mount as one wildcard route per HTTP method and cannot walk inside it,
// so the policy is declared for the subtree rather than for each leaf.
//
// That is a more accurate description of the guarantee anyway: the guard wraps
// the mount, so it applies to every path the subtree serves now and to any path
// added to it later, which is precisely the failure mode that left the docs tree
// unauthenticated.
var declaredSubtrees = map[string]string{
	"/api/docs/*": "member",
}

// A representative path inside each declared subtree, so the no-session test
// actually exercises the guard rather than trusting the declaration.
var subtreeProbes = map[string][]string{
	"/api/docs/*": {"/api/docs/", "/api/docs/llms.txt", "/api/docs/quickstart"},
}

// Every path inside a guarded subtree must refuse an anonymous caller. This is
// the test that would have caught the docs tree: it probes real paths rather
// than reading the registration.
func TestGuardedSubtreesRejectAnonymousCallers(t *testing.T) {
	mux := newTestServer().Mux()

	for subtree, policy := range declaredSubtrees {
		if policy == "public" {
			continue
		}
		for _, path := range subtreeProbes[subtree] {
			t.Run(path, func(t *testing.T) {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
					t.Errorf("%s served %d to an anonymous caller, want 401/403: %s",
						path, rec.Code, strings.TrimSpace(rec.Body.String()))
				}
			})
		}
	}
}

// The opt-in escape hatch must actually open the subtree, or an operator who
// chose a public docs site would get a broken one.
func TestPublicDocsOptInOpensTheDocsSubtree(t *testing.T) {
	srv := newTestServer()
	srv.SetPublicDocs(true)
	mux := srv.Mux()

	for _, path := range subtreeProbes["/api/docs/*"] {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("with publicDocs on, %s still refused (%d)", path, rec.Code)
		}
	}
}

// concreteURL turns a chi pattern into a requestable path.
func concreteURL(pattern string) string {
	out := []string{}
	for _, seg := range strings.Split(pattern, "/") {
		switch {
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			out = append(out, "00000000-0000-0000-0000-000000000000")
		default:
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// A request with no session must never reach a member or admin handler.
//
// The server is built with a nil store, so any handler that IS reached answers
// 503 ("control plane disabled"). That makes this test sharp in the right
// direction: 503 proves the guard did not run, because the guard would have
// answered 401 before the handler could look at the store.
func TestNoSessionIsRejectedOnEveryGuardedRoute(t *testing.T) {
	mux := newTestServer().Mux()

	for _, rp := range consoleRoutePolicies {
		if rp.policy == "public" {
			continue
		}
		t.Run(rp.method+" "+rp.path, func(t *testing.T) {
			req := httptest.NewRequest(rp.method, concreteURL(rp.path), strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			switch rec.Code {
			case http.StatusUnauthorized, http.StatusForbidden:
				// Guard fired before the handler. Correct.
			case http.StatusNotFound, http.StatusMethodNotAllowed:
				if conditionalRoutes[rp.method+" "+rp.path] {
					t.Skip("route needs an optional dependency that a bare test server does not wire")
				}
				t.Errorf("route is not registered (%d); the policy table and the mux disagree", rec.Code)
			default:
				t.Errorf("unauthenticated request got %d, want 401/403 — the handler ran without a session: %s",
					rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		})
	}
}

// Public routes must stay reachable. Locking down an endpoint that the login
// page or an invitee needs would be an outage, so the table pins both directions.
func TestPublicRoutesRemainReachableWithoutASession(t *testing.T) {
	mux := newTestServer().Mux()

	for _, rp := range consoleRoutePolicies {
		if rp.policy != "public" {
			continue
		}
		t.Run(rp.method+" "+rp.path, func(t *testing.T) {
			if rp.why == "" {
				t.Error("a public route must document why it is public")
			}
			req := httptest.NewRequest(rp.method, concreteURL(rp.path), strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			// Distinguish "you need a session" from "this feature is off".
			// Registration answers 403 when NEXUS_ALLOW_SIGNUP is false, which
			// is a feature gate, not an auth gate - and the distinction is
			// exactly what this test is for.
			body := strings.TrimSpace(rec.Body.String())
			authRefusal := strings.Contains(body, "login required") ||
				strings.Contains(body, "admin role required")
			if rec.Code == http.StatusUnauthorized || authRefusal {
				t.Errorf("public route now demands a session (%d): %s", rec.Code, body)
			}
		})
	}
}

// The policy table must describe the mux, not an idealised version of it. chi
// can walk its own routing tree, so any route the mux serves that nobody
// classified shows up here.
func TestEveryRouteHasADeclaredPolicy(t *testing.T) {
	for _, key := range undeclaredRoutes(walkConsoleRoutes(t)) {
		t.Errorf("route %q is served by the mux but has no declared policy; "+
			"add it to consoleRoutePolicies as public/member/admin", key)
	}
}

// undeclaredRoutes returns the walked routes that no policy covers. Factored out
// of the test above so the mechanism itself can be tested — see
// TestUndeclaredRouteIsReported.
func undeclaredRoutes(walked []string) []string {
	declared := map[string]bool{}
	for _, rp := range consoleRoutePolicies {
		declared[rp.method+" "+rp.path] = true
	}

	var missing []string
	for _, key := range walked {
		// The SPA catch-all and the /v1 gateway proxy are not console API
		// routes; they are asset serving and a reverse proxy with its own auth.
		if strings.HasSuffix(key, " /*") || strings.Contains(key, "/v1") {
			continue
		}
		if coveredBySubtree(key) {
			continue
		}
		if !declared[key] {
			missing = append(missing, key)
		}
	}
	return missing
}

// coveredBySubtree reports whether a walked wildcard route is one of the
// declared mounted subtrees.
func coveredBySubtree(key string) bool {
	for subtree := range declaredSubtrees {
		if strings.HasSuffix(key, " "+subtree) {
			return true
		}
	}
	return false
}
