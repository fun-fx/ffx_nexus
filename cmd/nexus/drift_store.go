package main

import (
	"context"
	"errors"
	"time"

	"github.com/ffxnexus/nexus/internal/benchmark"
	"github.com/ffxnexus/nexus/internal/core"
)

// driftStore adapts core.Store to benchmark.DriftSources.
//
// We expose a narrow view because the watcher only reads id, model,
// avg_score and completed_at — pulling in the whole BenchmarkRun
// would carry enough surface for a reviewer to mistake the watcher
// for something that mutates rows. It does not.
type driftStore struct {
	store *core.Store
}

// Reliably a guard, not a control-flow signal.
var _ benchmark.DriftSources = driftStore{}

func (d driftStore) GetBenchmarkRun(ctx context.Context, id string) (benchmark.RunLite, error) {
	if d.store == nil {
		return benchmark.RunLite{}, errors.New("drift: store not configured")
	}
	r, err := d.store.GetBenchmarkRun(ctx, id)
	if err != nil {
		return benchmark.RunLite{}, err
	}
	return benchmark.RunLite{
		ID:          r.ID,
		Model:       r.Model,
		AvgScore:    derefFloat(r.AvgScore),
		CompletedAt: dereqTime(r.CompletedAt),
	}, nil
}

func (d driftStore) ListRecentSettledRuns(ctx context.Context, model string, limit int) ([]benchmark.RunLite, error) {
	if d.store == nil {
		return nil, errors.New("drift: store not configured")
	}
	// Installation-wide on purpose: the drift watcher asks "has this model's
	// quality moved", which is a question about a shared upstream, and it emits
	// an operator alert rather than a per-tenant API response.
	rows, err := d.store.ListRecentSettledByModel(ctx, "", model, limit)
	if err != nil {
		return nil, err
	}
	out := make([]benchmark.RunLite, 0, len(rows))
	for _, r := range rows {
		out = append(out, benchmark.RunLite{
			ID:          r.ID,
			Model:       r.Model,
			AvgScore:    derefFloat(r.AvgScore),
			CompletedAt: dereqTime(r.CompletedAt),
		})
	}
	return out, nil
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func dereqTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}
