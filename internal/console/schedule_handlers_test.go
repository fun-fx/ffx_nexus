package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/benchmark"
	"github.com/ffxnexus/nexus/internal/core"
)

// scheduleStub is an in-memory BenchmarkRunner for the schedule
// handlers' tests. The non-schedule methods are stubs because the
// schedule handlers do not call them.
type scheduleStub struct {
	runs        []core.BenchmarkSchedule
	createErr   error
	listErr     error
	getErr      error
	deleteErr   error
	lastCreated core.BenchmarkSchedule
}

func (s *scheduleStub) Launch(_ context.Context, _ benchmark.LaunchSpec) (core.BenchmarkRun, error) {
	return core.BenchmarkRun{}, nil
}
func (s *scheduleStub) List(_ context.Context, _ string, _ int) ([]core.BenchmarkRun, error) {
	return nil, nil
}
func (s *scheduleStub) Get(_ context.Context, _ string) (core.BenchmarkRun, error) {
	return core.BenchmarkRun{}, nil
}
func (s *scheduleStub) Cancel(_ context.Context, _ string) error         { return nil }
func (s *scheduleStub) Delete(_ context.Context, _ string) error        { return nil }
func (s *scheduleStub) Logs(_ context.Context, _ string) (string, error) { return "", nil }
func (s *scheduleStub) Models(_ context.Context) ([]benchmark.Model, error) {
	return nil, nil
}
func (s *scheduleStub) PollOnce(_ context.Context) (int, error) { return 0, nil }
func (s *scheduleStub) GatewayRoutingAvailable() bool            { return true }
func (s *scheduleStub) DryRun(_ context.Context, _ benchmark.LaunchSpec) (benchmark.DryRunResult, error) {
	return benchmark.DryRunResult{}, nil
}

func (s *scheduleStub) CreateSchedule(_ context.Context, row core.BenchmarkSchedule) (core.BenchmarkSchedule, error) {
	if s.createErr != nil {
		return core.BenchmarkSchedule{}, s.createErr
	}
	if row.ID == "" {
		row.ID = "schd-auto"
	}
	s.lastCreated = row
	s.runs = append(s.runs, row)
	return row, nil
}

func (s *scheduleStub) ListSchedules(_ context.Context, _ string, _ int) ([]core.BenchmarkSchedule, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.runs, nil
}

func (s *scheduleStub) GetSchedule(_ context.Context, id string) (core.BenchmarkSchedule, error) {
	if s.getErr != nil {
		return core.BenchmarkSchedule{}, s.getErr
	}
	for _, r := range s.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return core.BenchmarkSchedule{}, errors.New("not found")
}

func (s *scheduleStub) DeleteSchedule(_ context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	for i, r := range s.runs {
		if r.ID == id {
			s.runs = append(s.runs[:i], s.runs[i+1:]...)
			return nil
		}
	}
	return errors.New("not found")
}

func (s *scheduleStub) GetLatestSettledByModel(_ context.Context, _ string) (core.BenchmarkRun, error) {
	return core.BenchmarkRun{}, errors.New("not found")
}

func (s *scheduleStub) ListRecentSettledByModel(_ context.Context, _ string, _ int) ([]core.RecentBenchmarkRun, error) {
	return nil, nil
}

// newScheduleServer is reserved for shared test wiring tests below.
// It is currently unused, kept as a hook if more integration tests
// grow on top of these handlers. We deliberately do not wrap the
// router here because each test below wires its own router to keep
// the failure mode (missing field, 4xx vs 5xx) explicit.
func newScheduleServer(stub *scheduleStub) *Server {
	s := &Server{}
	s.SetBenchmarks(stub)
	return s
}

// installScheduleRoutes returns a fresh router that exposes only the
// schedule endpoints under test. Keeping it private to this file
// stops any other test from quietly affecting the route map during a
// full Server suite run.
//
// The handler methods are admin-only in production; the requireAdmin
// middleware wraps them with the core.User guard. In tests we do not
// care about authorization — the route map is what we want to assert
// against — so we go through chi.URLParam directly rather than
// re-implementing the admin gate.
func (s *Server) installScheduleRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/api/eval/benchmarks/schedules/", func(w http.ResponseWriter, req *http.Request) {
		s.listBenchmarkSchedules(w, req, core.User{Email: "test@local"})
	})
	r.Post("/api/eval/benchmarks/schedules/", func(w http.ResponseWriter, req *http.Request) {
		s.createBenchmarkSchedule(w, req, core.User{Email: "test@local"})
	})
	r.Get("/api/eval/benchmarks/schedules/{id}", func(w http.ResponseWriter, req *http.Request) {
		s.getBenchmarkSchedule(w, req, core.User{Email: "test@local"})
	})
	r.Delete("/api/eval/benchmarks/schedules/{id}", func(w http.ResponseWriter, req *http.Request) {
		s.deleteBenchmarkSchedule(w, req, core.User{Email: "test@local"})
	})
	return r
}

func TestScheduleCreateHappyPath(t *testing.T) {
	stub := &scheduleStub{}
	srv := &Server{}
	srv.SetBenchmarks(stub)
	router := srv.installScheduleRoutes()

	body := bytes.NewReader([]byte(`{
		"name": "daily-gsm8k",
		"environments": ["primeintellect/gsm8k"],
		"model": "openai/gpt-4o-mini",
		"num_examples": 5,
		"rollouts": 1,
		"via_gateway": true,
		"cadence_seconds": 86400
	}`))
	req := httptest.NewRequest(http.MethodPost,
		"/api/eval/benchmarks/schedules/", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if stub.lastCreated.CadenceSeconds != 86400 {
		t.Fatalf("cadence: want 86400, got %d", stub.lastCreated.CadenceSeconds)
	}
	if stub.lastCreated.ID == "" {
		t.Fatal("runner did not receive an id")
	}
}

func TestScheduleCreateRejectsShortCadence(t *testing.T) {
	stub := &scheduleStub{}
	srv := &Server{}
	srv.SetBenchmarks(stub)

	body := bytes.NewReader([]byte(`{
		"name":"t","environments":["primeintellect/gsm8k"],
		"model":"openai/gpt-4o-mini","num_examples":5,"rollouts":1,
		"cadence_seconds":30
	}`))
	req := httptest.NewRequest(http.MethodPost,
		"/api/eval/benchmarks/schedules/", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.installScheduleRoutes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Drain the body so the test runner does not see a "no body" warning.
	_, _ = io.ReadAll(rec.Body)
}

func TestScheduleCreateMissingModelIsRejected(t *testing.T) {
	stub := &scheduleStub{}
	srv := &Server{}
	srv.SetBenchmarks(stub)

	body := bytes.NewReader([]byte(`{
		"name":"t","environments":["primeintellect/gsm8k"],
		"num_examples":5,"rollouts":1,"cadence_seconds":86400
	}`))
	req := httptest.NewRequest(http.MethodPost,
		"/api/eval/benchmarks/schedules/", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.installScheduleRoutes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestScheduleListRoundtripsListHandler(t *testing.T) {
	stub := &scheduleStub{runs: []core.BenchmarkSchedule{
		{ID: "a", Model: "openai/gpt-4o-mini"},
		{ID: "b", Model: "anthropic/claude-3-5-sonnet"},
	}}
	srv := &Server{}
	srv.SetBenchmarks(stub)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/eval/benchmarks/schedules/", nil)
	srv.installScheduleRoutes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var resp struct {
		Schedules []core.BenchmarkSchedule `json:"schedules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Schedules) != 2 {
		t.Fatalf("want 2 schedules, got %d", len(resp.Schedules))
	}
}

func TestScheduleGetMissingReturns404(t *testing.T) {
	stub := &scheduleStub{}
	srv := &Server{}
	srv.SetBenchmarks(stub)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/eval/benchmarks/schedules/nope", nil)
	srv.installScheduleRoutes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestScheduleDeleteUnknownReturnsError(t *testing.T) {
	stub := &scheduleStub{}
	srv := &Server{}
	srv.SetBenchmarks(stub)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/eval/benchmarks/schedules/nope", nil)
	srv.installScheduleRoutes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Suppress unused field warning — newScheduleServer is reserved for
// future test wiring, kept here so tests that follow this pattern
// have something to copy. The presence of a top-level router field on
// Server would change every existing benchmark test, so we keep it
// local.
var _ = newScheduleServer
