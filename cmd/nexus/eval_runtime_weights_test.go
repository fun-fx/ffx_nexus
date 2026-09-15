package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/router"
)

type emptyStats struct{}

func (emptyStats) ModelStats(context.Context, time.Duration) (map[string]router.ModelStats, error) {
	return map[string]router.ModelStats{}, nil
}

func TestEvalRuntimeApplyNestedRoutingWeights(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := evals.NewWorker(evals.Options{Workers: 1, PluginOnly: true}, log)
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	rt := router.New(emptyStats{}, router.DefaultWeights(), time.Hour, 0, log)
	t.Cleanup(rt.Close)

	c := &evalRuntimeController{worker: w, modelRouter: rt}
	q, cost, lat := 0.5, 0.3, 0.2
	snap, err := c.Apply(console.EvalConfigPatch{
		Routing: &console.EvalConfigPatchRouting{
			Weights: &console.EvalConfigPatchWeights{
				Quality: &q,
				Cost:    &cost,
				Latency: &lat,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := rt.Weights()
	if got.Quality != 0.5 || got.Cost != 0.3 || got.Latency != 0.2 {
		t.Fatalf("router weights = %+v, want 0.5/0.3/0.2", got)
	}
	if snap.Routing.Weights["quality"] != 0.5 || snap.Routing.Weights["cost"] != 0.3 {
		t.Fatalf("snapshot weights = %v", snap.Routing.Weights)
	}
}
