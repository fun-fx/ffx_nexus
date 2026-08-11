package benchmark

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSources struct {
	mu    sync.Mutex
	runs  map[string]map[string]RunLite // model -> ordered_by_completed_at_desc
}

func (f *fakeSources) GetBenchmarkRun(_ context.Context, id string) (RunLite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, byModel := range f.runs {
		for _, r := range byModel {
			if r.ID == id {
				return r, nil
			}
		}
	}
	return RunLite{}, errors.New("not found")
}

func (f *fakeSources) ListRecentSettledRuns(_ context.Context, model string, limit int) ([]RunLite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byModel, ok := f.runs[model]
	if !ok {
		return nil, nil
	}
	out := []RunLite{}
	for _, r := range byModel {
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeSources) put(runs map[string][]RunLite) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = map[string]map[string]RunLite{}
	for model, list := range runs {
		f.runs[model] = map[string]RunLite{}
		for _, r := range list {
			f.runs[model][r.ID] = r
		}
	}
}

type captureSink struct {
	mu     sync.Mutex
	alerts []DriftAlert
}

func (c *captureSink) Emit(_ context.Context, a DriftAlert) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alerts = append(c.alerts, a)
}

func (c *captureSink) snapshot() []DriftAlert {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]DriftAlert, len(c.alerts))
	copy(out, c.alerts)
	return out
}

func TestObserveSettleFiresAboveThreshold(t *testing.T) {
	src := &fakeSources{}
	src.put(map[string][]RunLite{
		"openai/gpt-4o-mini": {
			{ID: "older", Model: "openai/gpt-4o-mini", AvgScore: 0.50, CompletedAt: time.Now().Add(-2 * time.Hour)},
			{ID: "newer", Model: "openai/gpt-4o-mini", AvgScore: 0.80, CompletedAt: time.Now()},
		},
	})
	sink := &captureSink{}
	w := NewDriftWatcher(DriftAlertSpec{RelativeChangeThreshold: 0.05}, sink, nil)

	w.ObserveSettle(context.Background(), src, RunLite{
		ID: "newer", Model: "openai/gpt-4o-mini", AvgScore: 0.80, CompletedAt: time.Now(),
	})

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 alert, got %d", len(got))
	}
	if got[0].Kind != "relative_change" {
		t.Fatalf("kind: want relative_change, got %s", got[0].Kind)
	}
	if got[0].PreviousScore != 0.50 {
		t.Fatalf("previous: want 0.50, got %.2f", got[0].PreviousScore)
	}
}

func TestObserveSettleQuietBelowThreshold(t *testing.T) {
	src := &fakeSources{}
	src.put(map[string][]RunLite{
		"openai/gpt-4o-mini": {
			{ID: "older", AvgScore: 0.50, CompletedAt: time.Now().Add(-2 * time.Hour)},
			{ID: "newer", AvgScore: 0.51, CompletedAt: time.Now()},
		},
	})
	sink := &captureSink{}
	w := NewDriftWatcher(DriftAlertSpec{RelativeChangeThreshold: 0.10}, sink, nil)

	w.ObserveSettle(context.Background(), src, RunLite{
		ID: "newer", Model: "openai/gpt-4o-mini", AvgScore: 0.51, CompletedAt: time.Now(),
	})
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("want 0 alerts (under threshold), got %d", len(got))
	}
}

func TestObserveStalenessFiresWhenAgeExceedsThreshold(t *testing.T) {
	src := &fakeSources{}
	src.put(map[string][]RunLite{
		"old-model": {
			{ID: "stale", AvgScore: 0.6, CompletedAt: time.Now().Add(-60 * 24 * time.Hour)},
		},
	})
	sink := &captureSink{}
	w := NewDriftWatcher(DriftAlertSpec{FreshnessThreshold: 30 * 24 * time.Hour}, sink, nil)

	w.ObserveStaleness(context.Background(), src, []string{"old-model", "fresh-model"})

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 alert, got %d", len(got))
	}
	if got[0].Kind != "freshness_age" {
		t.Fatalf("kind: %s", got[0].Kind)
	}
	if got[0].Model != "old-model" {
		t.Fatalf("model: want old-model, got %s", got[0].Model)
	}
}

func TestObserveStalenessQuietWhenAbsent(t *testing.T) {
	src := &fakeSources{}
	sink := &captureSink{}
	w := NewDriftWatcher(DriftAlertSpec{FreshnessThreshold: 24 * time.Hour}, sink, nil)

	w.ObserveStaleness(context.Background(), src, []string{"never-benchmarked"})
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("want 0 alerts when there is no benchmark, got %d", len(got))
	}
}

func TestObserveSettleNoPriorDoesNotAlert(t *testing.T) {
	src := &fakeSources{}
	src.put(map[string][]RunLite{
		"fresh-model": {
			{ID: "only", AvgScore: 0.4, CompletedAt: time.Now()},
		},
	})
	sink := &captureSink{}
	w := NewDriftWatcher(DriftAlertSpec{RelativeChangeThreshold: 0.05}, sink, nil)

	w.ObserveSettle(context.Background(), src, RunLite{
		ID: "only", Model: "fresh-model", AvgScore: 0.4, CompletedAt: time.Now(),
	})
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("want 0 alerts when there is no prior, got %d", len(got))
	}
}
