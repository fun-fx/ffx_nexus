package cron

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeStore records the schedules that ListDueSchedules returned and
// every UpdateNextLaunchAt / MarkLaunched call so tests can assert on
// the runner's effect on the schedule state machine.
type fakeStore struct {
	mu      sync.Mutex
	due     []Spec
	updates []updateCall
	mark    []markCall

	// failNextList lets a test inject a transient error in the next
	// ListDueSchedules call only; cleared on first use. Helpful to
	// verify the runner does not abort the loop on a flaky read.
	failNextList bool
}

type updateCall struct {
	ID   string
	When time.Time
}

type markCall struct {
	ScheduleID string
	RunID      string
	When       time.Time
}

func (s *fakeStore) ListDueSchedules(_ context.Context, _ time.Time, _ int) ([]Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextList {
		s.failNextList = false
		return nil, errors.New("simulated")
	}
	// Copy because callers mutate the slice in place in some tests.
	out := make([]Spec, len(s.due))
	copy(out, s.due)
	return out, nil
}

func (s *fakeStore) UpdateNextLaunchAt(_ context.Context, id string, when time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, updateCall{ID: id, When: when})
	return nil
}

func (s *fakeStore) MarkLaunched(_ context.Context, sched, run string, when time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mark = append(s.mark, markCall{ScheduleID: sched, RunID: run, When: when})
	return nil
}

func (s *fakeStore) GetScheduleByID(_ context.Context, id string) (Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, spec := range s.due {
		if spec.ID == id {
			return spec, nil
		}
	}
	for _, u := range s.updates {
		if u.ID == id {
			for _, spec := range s.due {
				if spec.ID == id {
					spec.NextLaunchAt = u.When
					return spec, nil
				}
			}
		}
	}
	return Spec{}, errors.New("not found")
}

// stubLander returns runID when Land returns nil. Tests can swap in a
// function that returns an error to drive the failure path.
type stubLander struct {
	mu       sync.Mutex
	calls    []Spec
	respond  func(Spec) (string, error)
	nextFail bool
}

func (s *stubLander) RunSchedule(_ context.Context, spec Spec) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, spec)
	if s.nextFail {
		s.nextFail = false
		return "", errors.New("simulated launch failure")
	}
	if s.respond != nil {
		return s.respond(spec)
	}
	return "run-" + spec.ID, nil
}

// silentLogger discards so tests do not print noise.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func dueSchedule(id string) Spec {
	return Spec{
		ID:           id,
		OrgID:        "org-x",
		Name:         "daily-gsm8k",
		Environments: []string{"primeintellect/gsm8k"},
		Model:        "openai/gpt-4o-mini",
		NumExamples:  5,
		Rollouts:     1,
		ViaGateway:   true,
		Cadence:      time.Minute,
		NextLaunchAt: time.Now().UTC().Add(-time.Second),
	}
}

func TestTick_FiringReStampsNextLaunch(t *testing.T) {
	store := &fakeStore{due: []Spec{dueSchedule("s1")}}
	lander := &stubLander{}
	r := New(store, lander, silentLogger())

	r.tick(context.Background())

	if got, want := len(lander.calls), 1; got != want {
		t.Fatalf("lander call count: want %d, got %d", want, got)
	}
	if got, want := len(store.updates), 1; got != want {
		t.Fatalf("re-stamp count: want %d, got %d", want, got)
	}
	u := store.updates[0]
	if u.ID != "s1" {
		t.Fatalf("re-stamped id: want s1, got %s", u.ID)
	}
	if delta := time.Until(u.When); delta < 50*time.Second {
		// Cadence was 1 minute. Allow some slack for clock drift
		// between now() stamped in fire() and the test reading it.
		t.Fatalf("next launch later than cadence minus drift: %v", delta)
	}
	if got, want := len(store.mark), 1; got != want {
		t.Fatalf("mark-launches count: want %d, got %d", want, got)
	}
	if got, want := store.mark[0].RunID, "run-s1"; got != want {
		t.Fatalf("mark run id: want %s, got %s", want, got)
	}
	if r.LastError("s1") != "" {
		t.Fatalf("LastError after success must be empty, got %q", r.LastError("s1"))
	}
}

func TestTick_LaunchFailureStillAdvancesCadence(t *testing.T) {
	s := dueSchedule("s2")
	store := &fakeStore{due: []Spec{s}}
	lander := &stubLander{nextFail: true}
	r := New(store, lander, silentLogger())

	r.tick(context.Background())

	if got, want := len(store.updates), 1; got != want {
		t.Fatalf("re-stamp count after failure: want %d, got %d", want, got)
	}
	if r.LastError("s2") == "" {
		t.Fatal("LastError after failure must be set")
	}
}

func TestTick_ListErrorAbortsScan(t *testing.T) {
	store := &fakeStore{due: []Spec{dueSchedule("s3")}, failNextList: true}
	lander := &stubLander{}
	r := New(store, lander, silentLogger())

	r.tick(context.Background())

	if got := len(lander.calls); got != 0 {
		t.Fatalf("lander should not be called when list errors, got %d calls", got)
	}
	if got := len(store.updates); got != 0 {
		t.Fatalf("no re-stamp on list error, got %d", got)
	}
}

func TestTick_MultipleDueSchedulesAllFire(t *testing.T) {
	store := &fakeStore{due: []Spec{dueSchedule("a"), dueSchedule("b"), dueSchedule("c")}}
	lander := &stubLander{}
	r := New(store, lander, silentLogger())

	r.tick(context.Background())

	if got, want := len(lander.calls), 3; got != want {
		t.Fatalf("lander calls: want %d, got %d", want, len(lander.calls))
	}
	if got, want := len(store.updates), 3; got != want {
		t.Fatalf("re-stamps: want %d, got %d", want, got)
	}
}

func TestRun_ShutsDownOnContextCancel(t *testing.T) {
	store := &fakeStore{}
	lander := &stubLander{}
	r := New(store, lander, silentLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}
