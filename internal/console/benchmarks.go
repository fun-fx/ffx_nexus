package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/benchmark"
	"github.com/ffxnexus/nexus/internal/core"
)

// Benchmark runs: model-level quality measurements executed by an
// external platform.
//
// Every route is admin-only. Launching a run spends money at the
// provider and, when routed through the gateway, mints a credential that
// lets their sandbox call us — neither belongs behind a read-scoped role.

// BenchmarkRunner is the console's view of the benchmark lifecycle.
// Implemented by *benchmark.Runner; an interface so the handlers can be
// tested without a provider or a database.
type BenchmarkRunner interface {
	Launch(ctx context.Context, spec benchmark.LaunchSpec) (core.BenchmarkRun, error)
	List(ctx context.Context, orgID string, limit int) ([]core.BenchmarkRun, error)
	Get(ctx context.Context, id string) (core.BenchmarkRun, error)
	Cancel(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	Logs(ctx context.Context, id string) (string, error)
	Models(ctx context.Context) ([]benchmark.Model, error)
	PollOnce(ctx context.Context) (int, error)
	GatewayRoutingAvailable() bool
	DryRun(ctx context.Context, spec benchmark.LaunchSpec) (benchmark.DryRunResult, error)

	// Schedules: re-fire plans driven by internal/cron. The runner
	// exposes them as plain CRUD so the console can list, create and
	// delete without a separate dependency on internal/cron.
	//
	// SetScheduleEnabled toggles a row on or off without touching the
	// cadence or the run shape — those are still edited via
	// delete-then-create, per the design note in schedule_handlers.go.
	CreateSchedule(ctx context.Context, row core.BenchmarkSchedule) (core.BenchmarkSchedule, error)
	ListSchedules(ctx context.Context, orgID string, limit int) ([]core.BenchmarkSchedule, error)
	GetSchedule(ctx context.Context, id string) (core.BenchmarkSchedule, error)
	DeleteSchedule(ctx context.Context, id string) error
	SetScheduleEnabled(ctx context.Context, id string, enabled bool, nextLaunchAt time.Time) error

	// GetLatestSettledByModel returns the most-recent settled run
	// for a model, including MinScore / MaxScore / TotalSamples.
	// Used by the leaderboard to surface the full distribution
	// alongside AvgScore so the operator can see "the average is
	// X but the spread was wide" without opening the row detail.
	// Both take the caller's org: a benchmark score reflects the tenant's own
	// spec, dataset and provider key, so it is their data rather than shared
	// upstream health. See core.Store.ListRecentSettledByModel.
	GetLatestSettledByModel(ctx context.Context, orgID, model string) (core.BenchmarkRun, error)
	ListRecentSettledByModel(ctx context.Context, orgID, model string, limit int) ([]core.RecentBenchmarkRun, error)
}

// SetBenchmarks wires the runner. Left nil, the routes answer 503 so the
// console can explain the feature is unconfigured rather than 404.
func (s *Server) SetBenchmarks(r BenchmarkRunner) { s.benchmarks = r }

type launchBenchmarkRequest struct {
	Name           string   `json:"name"`
	Environments   []string `json:"environments"`
	Model          string   `json:"model"`
	NumExamples    int      `json:"num_examples"`
	Rollouts       int      `json:"rollouts"`
	TimeoutMinutes int      `json:"timeout_minutes"`
	ViaGateway     bool     `json:"via_gateway"`
}

func (s *Server) benchmarksReady(w http.ResponseWriter) bool {
	if s.benchmarks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "benchmarks are not configured: this deployment has no control-plane database",
		})
		return false
	}
	return true
}

func (s *Server) listBenchmarks(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.benchmarks.List(r.Context(), orgID(r), limit)
	if err != nil {
		s.failBenchmark(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs": runs,
		// The form needs to know whether it may offer gateway routing
		// before the operator submits and hits a 400.
		"gateway_routing_available": s.benchmarks.GatewayRoutingAvailable(),
		"max_total_samples":         benchmark.MaxTotalSamples,
	})
}

func (s *Server) launchBenchmark(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	var body launchBenchmarkRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	// The provider's own defaults are 5 examples and 3 rollouts; we
	// default rollouts to 1 instead because their docs recommend
	// proving a setup with the smallest possible run first.
	if body.NumExamples == 0 {
		body.NumExamples = 5
	}
	if body.Rollouts == 0 {
		body.Rollouts = 1
	}

	run, err := s.benchmarks.Launch(r.Context(), benchmark.LaunchSpec{
		// The signed-in user's org owns the run, not the X-Org-Id
		// header: a gateway-routed run mints a virtual key, and that key
		// has to belong to an org the gateway will authenticate against.
		OrgID:          benchmarkOrg(u, r),
		ActorID:        u.ID,
		Name:           body.Name,
		Environments:   body.Environments,
		Model:          body.Model,
		NumExamples:    body.NumExamples,
		Rollouts:       body.Rollouts,
		TimeoutMinutes: body.TimeoutMinutes,
		ViaGateway:     body.ViaGateway,
	})
	if err != nil {
		// A launch the provider refused is still recorded, so return the
		// row alongside the error and the console can show the failed
		// run instead of only a toast that disappears.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(benchmarkErrStatus(err))
		payload := map[string]any{"ok": false}
		if run.ID != "" {
			payload["run"] = run
		}
		// The cause is logged by s.fail; we render the body manually here
		// because a partially-successful launch must include the run row.
		_ = json.NewEncoder(w).Encode(payload)
		s.log.Error("launch benchmark partial failure",
			"err", err, "run_id", run.ID, "org", orgID(r), "model", body.Model)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.benchmark.launch", run.ID, run.Model)
	writeJSON(w, http.StatusCreated, run)
}

// ownedBenchmark fetches the run named by {id} and confirms it belongs to the
// caller's org, writing the response and returning ok=false when it does not.
//
// BenchmarkRunner.Get/Cancel/Delete/Logs all key on the bare run id, so every
// per-run route was reachable across orgs: one team could read another team's
// run — its model, dataset, environments and provider logs — and cancel or
// delete it. Cancel and delete are the sharp end, since a benchmark costs money
// at the provider and cancelling it wastes that spend.
//
// The org check lives here rather than in the runner because the runner is also
// driven by the scheduler, which legitimately operates across orgs. The rule
// being enforced is about a request, not about the data.
func (s *Server) ownedBenchmark(w http.ResponseWriter, r *http.Request) (core.BenchmarkRun, bool) {
	run, err := s.benchmarks.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.failBenchmark(w, r, err)
		return core.BenchmarkRun{}, false
	}
	// A run recorded before org attribution existed has no org. Treat it as
	// belonging to the default org rather than to everyone.
	owner := run.OrgID
	if owner == "" {
		owner = core.DefaultOrgID
	}
	if owner != orgID(r) {
		// 404, not 403: confirming the id exists would let a caller enumerate
		// other teams' runs.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "benchmark run not found"})
		return core.BenchmarkRun{}, false
	}
	return run, true
}

func (s *Server) getBenchmark(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	run, ok := s.ownedBenchmark(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelBenchmark(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	run, ok := s.ownedBenchmark(w, r)
	if !ok {
		return
	}
	if err := s.benchmarks.Cancel(r.Context(), run.ID); err != nil {
		s.failBenchmark(w, r, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.benchmark.cancel", run.ID, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteBenchmark(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	run, ok := s.ownedBenchmark(w, r)
	if !ok {
		return
	}
	if err := s.benchmarks.Delete(r.Context(), run.ID); err != nil {
		s.failBenchmark(w, r, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.benchmark.delete", run.ID, "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) benchmarkLogs(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	run, ok := s.ownedBenchmark(w, r)
	if !ok {
		return
	}
	logs, err := s.benchmarks.Logs(r.Context(), run.ID)
	if err != nil {
		s.failBenchmark(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"logs": logs})
}

// benchmarkModels proxies the provider's inference catalogue so the
// console can offer a picker with pricing instead of a free-text field.
func (s *Server) benchmarkModels(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	models, err := s.benchmarks.Models(r.Context())
	if err != nil {
		s.failBenchmark(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// refreshBenchmarks forces a poll pass. The background ticker already
// does this, but a run can take hours and an operator watching the page
// should not have to guess whether the next tick has happened.
func (s *Server) refreshBenchmarks(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	n, err := s.benchmarks.PollOnce(r.Context())
	if err != nil {
		s.failBenchmark(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": n})
}

// Provider credential. Stored through the same encrypted plugin-key
// vault under a reserved name, so it inherits that table's master-key
// encryption and survives a restart.
//
// These are separate routes rather than a reuse of
// /eval/plugins/{name}/keys because those handlers resolve the plugin
// first and 404 when there is no manifest — and a benchmark provider
// deliberately has no plugin row.

func (s *Server) benchmarkCredentialReady(w http.ResponseWriter) bool {
	if s.pluginKeys == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "credential storage is not wired",
		})
		return false
	}
	return true
}

// getBenchmarkCredential reports only whether a token is stored. The
// value never leaves the process, so the response is safe to log.
func (s *Server) getBenchmarkCredential(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.benchmarkCredentialReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider":   benchmark.ProviderPrime,
		"configured": s.pluginKeys.Has(benchmark.CredentialName),
		"team_id":    benchmarkStoredTeamID(s.pluginKeys),
	})
}

func benchmarkStoredTeamID(keys EvalPluginKeys) string {
	if keys == nil {
		return ""
	}
	kv, ok := keys.Get(benchmark.CredentialName)
	if !ok {
		return ""
	}
	return kv[benchmark.CredentialTeamIDKey]
}

func (s *Server) putBenchmarkCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.benchmarkCredentialReady(w) {
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
		TeamID string `json:"team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	existing, had := s.pluginKeys.Get(benchmark.CredentialName)
	kv := map[string]string{}
	if had {
		for k, v := range existing {
			kv[k] = v
		}
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey != "" {
		kv[benchmark.CredentialKey] = apiKey
	} else if kv[benchmark.CredentialKey] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "api_key is required"})
		return
	}
	teamID := strings.TrimSpace(body.TeamID)
	if teamID != "" {
		kv[benchmark.CredentialTeamIDKey] = teamID
	} else {
		delete(kv, benchmark.CredentialTeamIDKey)
	}
	if err := s.pluginKeys.Set(benchmark.CredentialName, kv); err != nil {
		// A write that only reached memory would work until the next
		// deploy and then fail silently, which is the exact trap the
		// plugin keys went through. Report it instead.
		s.failBenchmark(w, r, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.benchmark.credential.set", benchmark.ProviderPrime, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"configured": true,
		"team_id":    kv[benchmark.CredentialTeamIDKey],
	})
}

func (s *Server) deleteBenchmarkCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.benchmarkCredentialReady(w) {
		return
	}
	if err := s.pluginKeys.Clear(benchmark.CredentialName); err != nil {
		s.failBenchmark(w, r, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.benchmark.credential.clear", benchmark.ProviderPrime, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "configured": false})
}

// benchmarkOrg picks the org a run belongs to. The session's org wins
// because it is the one a minted gateway key can authenticate against;
// the header is only a fallback for deployments that set it explicitly.
func benchmarkOrg(u core.User, r *http.Request) string {
	if u.OrgID != "" {
		return u.OrgID
	}
	return orgID(r)
}

// dryRunBenchmark verifies the provider credential and the
// environment slugs return through one POST + one PATCH round-trip
// without persisting a row.
//
// The console uses this so an operator can sanity-check a slug
// before submitting a real launch. A 404 surfaces the vendor's
// reason in the response, which is the most common useful answer
// when the environment has not been published to this account yet.
func (s *Server) dryRunBenchmark(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.benchmarksReady(w) {
		return
	}
	var body struct {
		Environments   []string `json:"environments"`
		Model          string   `json:"model"`
		TimeoutMinutes int      `json:"timeout_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if out, err := s.benchmarks.DryRun(r.Context(), benchmark.LaunchSpec{
		Environments:   body.Environments,
		Model:          body.Model,
		TimeoutMinutes: body.TimeoutMinutes,
	}); err != nil {
		// Dry-run failures still log so an operator can ask support using
		// the request id; the body carries only the boolean failure flag
		// and any warnings that don't expose the cause.
		s.log.Error("benchmark dry-run failed", "err", err, "model", body.Model, "org", orgID(r))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(benchmarkErrStatus(err))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
		return
	} else {
		resp := map[string]any{"ok": true}
		if out.Warning != "" {
			resp["warning"] = out.Warning
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
}

// benchmarkErrStatus maps a runner error onto a status code. Missing
// credentials and rejected input are the operator's to fix, so they must
// not read as a server fault.
func benchmarkErrStatus(err error) int {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, benchmark.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, benchmark.ErrNoToken), errors.Is(err, benchmark.ErrInvalidRequest):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// benchmarkErrCode maps the sentinel error to a public apierr.Code, matching
// benchmarkErrStatus's status. The code is what the body returns; the status
// is the HTTP envelope. Two functions on purpose: the same mapping rule for
// both, but each is one line of switch.
// stripWrappedLeaf walks the error chain and returns the operator-facing leaf:
// the substring after the last sentinel colon, for an error wrapped via
// fmt.Errorf("%w: <leaf>", ErrInvalidRequest).
//
// The string is safe to surface because the runner only wraps ErrInvalidRequest
// with operator-readable leaves ("this run never started at the provider",
// "schedule id is required", "model is required"). If a future wrap adds a
// internal-looking leaf (a SQL error, a path), it would surface here, so
// internal/benchmark/runner.go is a sibling concern: never wrap ErrInvalidRequest
// with a string the operator should not see.
func stripWrappedLeaf(err error) string {
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		s := cur.Error()
		if i := strings.LastIndex(s, ": "); i >= 0 {
			cand := strings.TrimSpace(s[i+2:])
			// Skip the sentinel identifier itself (its Error() is "benchmark: invalid
			// request"); skip obvious sentinel/system wrappers; the leaf is the part
			// after the colon that does not contain another colon-joined token.
			if cand != "" {
				// Take anything after the LAST ": ", e.g.
				// "benchmark: invalid request: this run never started" -> "this run never started"
				// which is the desired leaf.
				return cand
			}
		}
	}
	return ""
}

func benchmarkErrCode(err error) apierr.Code {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return apierr.CodeNotFound
	case errors.Is(err, benchmark.ErrConflict):
		return apierr.CodeConflict
	case errors.Is(err, benchmark.ErrNoToken), errors.Is(err, benchmark.ErrInvalidRequest):
		return apierr.CodeInvalidRequest
	}
	return apierr.CodeInternalError
}

// failBenchmark converts the (status, code) the sentinel would pick into a
// public response. When the sentinel carries an operator-facing message
// (ErrInvalidRequest with a leaf, ErrNoToken, etc.), failBenchmark surfaces it
// in the body so the operator can act without checking a log. When the
// sentinel doesn't (a generic internal error), it goes through the standard
// s.fail and the body shows only the generic message.
func (s *Server) failBenchmark(w http.ResponseWriter, r *http.Request, err error) {
	msg := benchmarkErrMessage(err)
	status := benchmarkErrStatus(err)
	code := benchmarkErrCode(err)
	if msg != "" {
		s.failWithMessage(w, r, status, code, msg, err)
		return
	}
	s.fail(w, r, status, code, err)
}

// benchmarkErrMessage returns the public-safe caller-facing message for a
// sentinel error. For sentinel-bounded errors, the operator wants to see the
// reason in the body so they can fix the input without checking a log. The
// calling code passes the message directly via s.failWithMessage.
//
// We deliberately do NOT use err.Error() here. The unwrap chain for the
// sentinel-bounded cases here is operator-friendly: "benchmark: invalid
// request: this run never started at the provider". The "benchmark:" /
// "invalid request:" prefixes are derived from sentinel-wrap bookkeeping in
// the runner; only the leaf string contains user-facing context. We drop the
// wrapper text by rewrapping a clean tag around the leaf.
//
// If a future sentinel adds an internal-looking message here, replace this
// with a hand-rolled string per case.
func benchmarkErrMessage(err error) string {
	if errors.Is(err, core.ErrNotFound) {
		return "the requested benchmark run was not found"
	}
	if errors.Is(err, benchmark.ErrConflict) {
		return "the benchmark run is already in a terminal state"
	}
	if errors.Is(err, benchmark.ErrNoToken) {
		return "a benchmark provider credential is not configured"
	}
	if errors.Is(err, benchmark.ErrInvalidRequest) {
		// The wrap chain for ErrInvalidRequest carries the leaf at the top:
		// fmt.Errorf("%w: this run never started at the provider", ErrInvalidRequest)
		// produces err.Error() = "benchmark: invalid request: this run never started at the provider"
		// with err.Unwrap().Error() = "benchmark: invalid request". The leaf the
		// operator needs is the substring after the LAST "wrapped-sentinel: " —- i.e.,
		// strip everything up to and including the last ": " before the operator
		// facing message. The current buyer's path uses ": this run never started
		// at the provider" / ": schedule id is required" / ": model is required"
		// — all non-protected substrings.
		leaf := stripWrappedLeaf(err)
		if leaf != "" {
			return leaf
		}
		return "the benchmark input is invalid"
	}
	return ""
}
