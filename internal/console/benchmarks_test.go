package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/benchmark"
	"github.com/ffxnexus/nexus/internal/core"
)

// fakeRunner records what the handlers asked for and returns scripted
// results, so these tests cover the HTTP contract without a provider.
type fakeRunner struct {
	lastSpec  benchmark.LaunchSpec
	run       core.BenchmarkRun
	runs      []core.BenchmarkRun
	launchErr error
	cancelErr error
	deleteErr error
	getErr    error
	logs      string
	logsErr   error
	models    []benchmark.Model
	polled    int
	gateway   bool
	cancelled []string
	deleted   []string
}

func (f *fakeRunner) Launch(_ context.Context, spec benchmark.LaunchSpec) (core.BenchmarkRun, error) {
	f.lastSpec = spec
	return f.run, f.launchErr
}
func (f *fakeRunner) List(_ context.Context, _ string, _ int) ([]core.BenchmarkRun, error) {
	return f.runs, nil
}
func (f *fakeRunner) Get(_ context.Context, _ string) (core.BenchmarkRun, error) {
	return f.run, f.getErr
}
func (f *fakeRunner) Cancel(_ context.Context, id string) error {
	f.cancelled = append(f.cancelled, id)
	return f.cancelErr
}
func (f *fakeRunner) Delete(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}
func (f *fakeRunner) Logs(_ context.Context, _ string) (string, error) { return f.logs, f.logsErr }
func (f *fakeRunner) Models(_ context.Context) ([]benchmark.Model, error) {
	return f.models, nil
}
func (f *fakeRunner) PollOnce(_ context.Context) (int, error) { f.polled++; return 3, nil }
func (f *fakeRunner) GatewayRoutingAvailable() bool           { return f.gateway }

// fakeKeyStore stands in for the encrypted plugin-key vault.
type fakeKeyStore struct {
	kv     map[string]map[string]string
	setErr error
}

func newFakeKeyStore() *fakeKeyStore {
	return &fakeKeyStore{kv: map[string]map[string]string{}}
}
func (f *fakeKeyStore) Get(plugin string) (map[string]string, bool) {
	v, ok := f.kv[plugin]
	return v, ok
}
func (f *fakeKeyStore) Set(plugin string, kv map[string]string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.kv[plugin] = kv
	return nil
}
func (f *fakeKeyStore) Clear(plugin string) error { delete(f.kv, plugin); return nil }
func (f *fakeKeyStore) Has(plugin string) bool    { _, ok := f.kv[plugin]; return ok }

func benchServer(runner BenchmarkRunner, keys EvalPluginKeys) *Server {
	s := NewServer(nil, nil, nil, slog.Default())
	s.SetBenchmarks(runner)
	if keys != nil {
		s.SetPluginKeys(keys)
	}
	return s
}

func admin() core.User {
	return core.User{ID: "user-1", OrgID: "org-a", Role: core.RoleAdmin}
}

func call(h func(http.ResponseWriter, *http.Request, core.User), method, target, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h(rec, r, admin())
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

func TestBenchmarkRoutesRequireAdminSession(t *testing.T) {
	// The whole surface spends money or mints credentials, so an
	// unauthenticated caller must never reach a handler.
	mux := benchServer(&fakeRunner{}, newFakeKeyStore()).Mux()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/eval/benchmarks"},
		{http.MethodPost, "/api/eval/benchmarks"},
		{http.MethodGet, "/api/eval/benchmarks/models"},
		{http.MethodPost, "/api/eval/benchmarks/refresh"},
		{http.MethodGet, "/api/eval/benchmarks/credential"},
		{http.MethodPut, "/api/eval/benchmarks/credential"},
		{http.MethodDelete, "/api/eval/benchmarks/credential"},
		{http.MethodGet, "/api/eval/benchmarks/run-1"},
		{http.MethodDelete, "/api/eval/benchmarks/run-1"},
		{http.MethodPost, "/api/eval/benchmarks/run-1/cancel"},
		{http.MethodGet, "/api/eval/benchmarks/run-1/logs"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}")))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestBenchmarkStaticPathsAreNotReadAsRunIDs(t *testing.T) {
	// /models and /credential must reach their own handlers rather than
	// being captured by /{id}.
	runner := &fakeRunner{
		models: []benchmark.Model{{ID: "openai/gpt-4o-mini"}},
		// A run id lookup would return this, which is how we detect
		// the wrong route winning.
		run: core.BenchmarkRun{ID: "should-not-be-used"},
	}
	srv := benchServer(runner, newFakeKeyStore())

	rec := call(srv.benchmarkModels, http.MethodGet, "/api/eval/benchmarks/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decode(t, rec)
	if _, ok := body["models"]; !ok {
		t.Fatalf("models response = %s", rec.Body.String())
	}
}

func TestListBenchmarksReportsFormCapabilities(t *testing.T) {
	runner := &fakeRunner{
		runs:    []core.BenchmarkRun{{ID: "r1", Model: "gpt-4o-mini", Status: "running"}},
		gateway: true,
	}
	rec := call(benchServer(runner, nil).listBenchmarks, http.MethodGet, "/api/eval/benchmarks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decode(t, rec)
	if body["gateway_routing_available"] != true {
		t.Error("the form needs to know gateway routing is available")
	}
	// Surfacing the cap lets the form warn before a submit is rejected.
	if body["max_total_samples"] != float64(benchmark.MaxTotalSamples) {
		t.Errorf("max_total_samples = %v", body["max_total_samples"])
	}
	if runs, ok := body["runs"].([]any); !ok || len(runs) != 1 {
		t.Errorf("runs = %v", body["runs"])
	}
}

func TestLaunchBenchmarkAppliesSmallestUsefulDefaults(t *testing.T) {
	runner := &fakeRunner{run: core.BenchmarkRun{ID: "r1"}}
	rec := call(benchServer(runner, nil).launchBenchmark, http.MethodPost, "/api/eval/benchmarks",
		`{"environments":["ffx/gsm8k"],"model":"gpt-4o-mini"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	// The provider defaults to 3 rollouts; we start at 1 so a first run
	// is as cheap as possible.
	if runner.lastSpec.NumExamples != 5 || runner.lastSpec.Rollouts != 1 {
		t.Errorf("defaults = %d examples × %d rollouts, want 5 × 1",
			runner.lastSpec.NumExamples, runner.lastSpec.Rollouts)
	}
	// The run must be owned by the session's org, not the header, so a
	// minted gateway key belongs somewhere the gateway will accept.
	if runner.lastSpec.OrgID != "org-a" {
		t.Errorf("org = %q, want the session org", runner.lastSpec.OrgID)
	}
	if runner.lastSpec.ActorID != "user-1" {
		t.Errorf("actor = %q", runner.lastSpec.ActorID)
	}
}

func TestLaunchBenchmarkPassesExplicitValuesThrough(t *testing.T) {
	runner := &fakeRunner{run: core.BenchmarkRun{ID: "r1"}}
	rec := call(benchServer(runner, nil).launchBenchmark, http.MethodPost, "/api/eval/benchmarks",
		`{"name":"nightly","environments":["ffx/a","ffx/b"],"model":"m",
		  "num_examples":20,"rollouts":2,"timeout_minutes":240,"via_gateway":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	s := runner.lastSpec
	if s.Name != "nightly" || len(s.Environments) != 2 || s.NumExamples != 20 ||
		s.Rollouts != 2 || s.TimeoutMinutes != 240 || !s.ViaGateway {
		t.Fatalf("spec = %+v", s)
	}
}

func TestLaunchBenchmarkErrorsMapToStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"bad input", fmt.Errorf("%w: model is required", benchmark.ErrInvalidRequest), http.StatusBadRequest},
		{"no credential", fmt.Errorf("%w: paste the key", benchmark.ErrNoToken), http.StatusBadRequest},
		{"already settled", fmt.Errorf("%w: run is already completed", benchmark.ErrConflict), http.StatusConflict},
		{"unknown run", core.ErrNotFound, http.StatusNotFound},
		{"provider fault", errors.New("benchmark: HTTP 500 from provider"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{launchErr: tc.err}
			rec := call(benchServer(runner, nil).launchBenchmark, http.MethodPost,
				"/api/eval/benchmarks", `{"environments":["e"],"model":"m"}`)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if decode(t, rec)["error"] == nil {
				t.Error("response carries no error message")
			}
		})
	}
}

func TestLaunchBenchmarkReturnsTheRecordedFailure(t *testing.T) {
	// A launch the provider refused is still stored. Returning the row
	// lets the page show the failed run instead of a vanishing toast.
	runner := &fakeRunner{
		run:       core.BenchmarkRun{ID: "r1", Status: "failed", Error: "Environment not found"},
		launchErr: errors.New("benchmark: not found (404)"),
	}
	rec := call(benchServer(runner, nil).launchBenchmark, http.MethodPost,
		"/api/eval/benchmarks", `{"environments":["ffx/nope"],"model":"m"}`)
	body := decode(t, rec)
	run, ok := body["run"].(map[string]any)
	if !ok {
		t.Fatalf("no run in the failure response: %s", rec.Body.String())
	}
	if run["status"] != "failed" {
		t.Errorf("run status = %v", run["status"])
	}
}

func TestLaunchBenchmarkRejectsBadJSON(t *testing.T) {
	rec := call(benchServer(&fakeRunner{}, nil).launchBenchmark, http.MethodPost,
		"/api/eval/benchmarks", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCancelAndDeleteReachTheRunner(t *testing.T) {
	runner := &fakeRunner{run: core.BenchmarkRun{ID: "r1", Status: "running"}}
	srv := benchServer(runner, nil)

	if rec := call(srv.cancelBenchmark, http.MethodPost, "/api/eval/benchmarks/r1/cancel", ""); rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := call(srv.deleteBenchmark, http.MethodDelete, "/api/eval/benchmarks/r1", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(runner.cancelled) != 1 || len(runner.deleted) != 1 {
		t.Errorf("cancelled=%v deleted=%v", runner.cancelled, runner.deleted)
	}
}

func TestCancelSettledRunIsAConflict(t *testing.T) {
	runner := &fakeRunner{cancelErr: fmt.Errorf("%w: run is already completed", benchmark.ErrConflict)}
	rec := call(benchServer(runner, nil).cancelBenchmark, http.MethodPost,
		"/api/eval/benchmarks/r1/cancel", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestLogsSurfacesTheReasonThereAreNone(t *testing.T) {
	runner := &fakeRunner{
		logsErr: fmt.Errorf("%w: this run never started at the provider", benchmark.ErrInvalidRequest),
	}
	rec := call(benchServer(runner, nil).benchmarkLogs, http.MethodGet,
		"/api/eval/benchmarks/r1/logs", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "never started") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestRefreshForcesAPollPass(t *testing.T) {
	runner := &fakeRunner{}
	rec := call(benchServer(runner, nil).refreshBenchmarks, http.MethodPost,
		"/api/eval/benchmarks/refresh", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if runner.polled != 1 {
		t.Errorf("polled %d times, want 1", runner.polled)
	}
	if decode(t, rec)["updated"] != float64(3) {
		t.Errorf("updated = %v", decode(t, rec)["updated"])
	}
}

func TestUnconfiguredBenchmarksAnswer503(t *testing.T) {
	// Nil runner means no Postgres. A 503 with an explanation beats a
	// 404 that looks like a broken build.
	srv := NewServer(nil, nil, nil, slog.Default())
	rec := call(srv.listBenchmarks, http.MethodGet, "/api/eval/benchmarks", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// --- provider credential ---

func TestCredentialRoundTripNeverReturnsTheValue(t *testing.T) {
	keys := newFakeKeyStore()
	srv := benchServer(&fakeRunner{}, keys)

	rec := call(srv.getBenchmarkCredential, http.MethodGet, "/api/eval/benchmarks/credential", "")
	if decode(t, rec)["configured"] != false {
		t.Error("a fresh install must report no credential")
	}

	rec = call(srv.putBenchmarkCredential, http.MethodPut, "/api/eval/benchmarks/credential",
		`{"api_key":"  pit_secret  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d: %s", rec.Code, rec.Body.String())
	}
	// Stored under the reserved name, trimmed, in the vault's key slot.
	got := keys.kv[benchmark.CredentialName][benchmark.CredentialKey]
	if got != "pit_secret" {
		t.Errorf("stored %q, want the trimmed key", got)
	}

	rec = call(srv.getBenchmarkCredential, http.MethodGet, "/api/eval/benchmarks/credential", "")
	body := rec.Body.String()
	if decode(t, rec)["configured"] != true {
		t.Error("credential not reported as configured")
	}
	if strings.Contains(body, "pit_secret") {
		t.Fatalf("the token must never be returned: %s", body)
	}

	rec = call(srv.deleteBenchmarkCredential, http.MethodDelete, "/api/eval/benchmarks/credential", "")
	if rec.Code != http.StatusOK || keys.Has(benchmark.CredentialName) {
		t.Errorf("delete left the credential behind (status %d)", rec.Code)
	}
}

func TestCredentialRejectsEmptyKey(t *testing.T) {
	srv := benchServer(&fakeRunner{}, newFakeKeyStore())
	for _, body := range []string{`{"api_key":""}`, `{"api_key":"   "}`, `{}`} {
		rec := call(srv.putBenchmarkCredential, http.MethodPut, "/api/eval/benchmarks/credential", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s gave status %d, want 400", body, rec.Code)
		}
	}
}

func TestCredentialReportsAFailedPersist(t *testing.T) {
	// A write that only reached memory would work until the next deploy
	// and then fail silently — the exact trap the plugin keys hit.
	keys := newFakeKeyStore()
	keys.setErr = errors.New("database is down")
	rec := call(benchServer(&fakeRunner{}, keys).putBenchmarkCredential,
		http.MethodPut, "/api/eval/benchmarks/credential", `{"api_key":"pit_x"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "database is down") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestCredentialWithoutVaultAnswers503(t *testing.T) {
	rec := call(benchServer(&fakeRunner{}, nil).getBenchmarkCredential,
		http.MethodGet, "/api/eval/benchmarks/credential", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
