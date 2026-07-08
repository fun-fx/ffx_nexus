package evals

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

type captureSink struct {
	mu     sync.Mutex
	scores []Score
}

func (s *captureSink) WriteScores(_ context.Context, scores []Score) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scores = append(s.scores, scores...)
	return nil
}

func (s *captureSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.scores)
}

// TestWorkerHeuristicsWithoutClickHouse verifies the eval engine runs heuristics
// with a non-ClickHouse sink (NoopSink path via captureSink).
func TestWorkerHeuristicsWithoutClickHouse(t *testing.T) {
	sink := &captureSink{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWorker(Options{
		PIIEnabled:          true,
		CompletenessEnabled: true,
		Sink:                sink,
		JudgeSampleRate:     0,
		Workers:             1,
		BufferSize:          4,
	}, log)

	w.Record(observability.Trace{
		TraceID:        "t1",
		StatusCode:     200,
		OutputMessages: "Contact me at john.doe@example.com",
	})

	deadline := time.Now().Add(2 * time.Second)
	for sink.len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sink.len() == 0 {
		t.Fatal("expected heuristic scores without clickhouse sink")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = w.Close(ctx)
}
