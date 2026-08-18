// Package health provides the process-wide readiness gate.
//
// The distinction between liveness and readiness matters operationally and was
// previously absent: the binary served only /healthz, which returned a literal
// "ok" as soon as the HTTP server was listening. That made every failure mode
// that leaves the process running but unable to do its job - an unmigrated
// schema, a missing control-plane database, per-pod-only rate limiting - look
// identical to a healthy pod. Kubernetes would route customer traffic to it.
//
//   - Liveness answers "is this process wedged, should the kubelet restart it".
//     Restarting does not fix a missing migration, so liveness must NOT depend
//     on dependencies; otherwise a database blip turns into a restart storm.
//   - Readiness answers "should this pod receive traffic right now". This is
//     where dependency and schema state belong, because the correct response to
//     "the schema is behind the binary" is to stop taking requests, not to die.
package health

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

// Check is one named readiness condition.
type Check struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Detail explains a failure in terms an operator can act on. It must never
	// contain a credential: this payload is reachable by anything that can
	// reach the pod's readiness port.
	Detail string `json:"detail,omitempty"`
	// Required distinguishes "this pod cannot serve" from "a feature is
	// degraded". A non-required check that fails is reported but does not
	// withhold traffic, which is what keeps an optional analytics store from
	// taking the gateway offline.
	Required bool `json:"required"`
}

// Gate collects readiness conditions. The zero value is usable and reports
// ready, so a caller that registers nothing behaves exactly as before.
type Gate struct {
	mu     sync.RWMutex
	checks map[string]Check
}

// New returns an empty Gate.
func New() *Gate { return &Gate{checks: map[string]Check{}} }

// Set records or replaces a condition. Safe for concurrent use so a background
// reconnect loop can flip a check without coordinating with the HTTP handler.
func (g *Gate) Set(name string, ok bool, required bool, detail string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.checks == nil {
		g.checks = map[string]Check{}
	}
	g.checks[name] = Check{Name: name, OK: ok, Detail: detail, Required: required}
}

// Ready reports whether every REQUIRED condition passes, along with all
// conditions for reporting.
func (g *Gate) Ready() (bool, []Check) {
	if g == nil {
		return true, nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := make([]Check, 0, len(g.checks))
	ready := true
	for _, c := range g.checks {
		if c.Required && !c.OK {
			ready = false
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return ready, out
}

// Handler serves the readiness probe: 200 when ready, 503 otherwise, with a
// JSON body naming which condition failed and why. The body is what turns
// "CrashLoopBackOff, good luck" into "postgres_schema: 1 migration pending
// (postgres/014_invite_tokens.sql); run the migration job".
func (g *Gate) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		ready, checks := g.Ready()
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(struct {
			Ready  bool    `json:"ready"`
			Checks []Check `json:"checks"`
		}{Ready: ready, Checks: checks})
	}
}
