//go:build !prodtest_disable_eval_seam

package console

import (
	"net/http"

	"github.com/ffxnexus/nexus/internal/core"
)

// TestPluginCreate exposes the createEvalPlugin handler so tests
// outside the package can verify the response envelope without
// going through the full admin-guard middleware (whose session
// resolution expects a non-nil store). The seam is intentionally
// build-tagged so prodtest_disable_eval_seam can hide it in CI
// lanes that want the public API surface reduced.
//
// The exported name is prefixed Test* so a future developer
// grepping for exported ident surfacing of internal-only paths
// sees the seam and is invited to consider whether they should
// be calling the layer that gates it instead.
func TestPluginCreate(srv *Server) func(http.ResponseWriter, *http.Request) {
	if srv == nil || srv.evalPlugins == nil {
		// Match the handler's own early-return so the test sees
		// the same 503 as the production route.
		return func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugins disabled"})
		}
	}
	admin := core.User{
		ID:    "test-admin",
		Role:  core.RoleAdmin,
		OrgID: "test-org",
	}
	// Closure that captures the admin user. Bypasses auth in
	// test mode only; never invoked outside tests.
	return func(w http.ResponseWriter, r *http.Request) {
		srv.createEvalPlugin(w, r, admin)
	}
}
