package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// OTLPEvaluationLogSink is the seam the Collector uses to fan
// `gen_ai.evaluation.result` events out to OTLP-aware collectors.
// Implementations must be safe for concurrent use — the Collector
// calls EmitShip from many worker goroutines. nil sinks must be
// tolerated by callers (no error, no log spam).
//
//   - HTTPLogSink: POSTs the OTLPEvaluationEventEnvelope to a
//     configurable OTLP/HTTP-compatible /v1/logs endpoint. This
//     is the one main.go constructs in production.
//
//   - NoopLogSink: a discard sink used when no OTLP collector is
//     configured.
type OTLPEvaluationLogSink interface {
	// EmitShip enqueues the event for the next flush. Errors from
	// the background POST do not propagate here — HTTPLogSink logs
	// at warn level and returns nil so the eval worker is never
	// blocked on a slow collector.
	EmitShip(ctx context.Context, traceID string, score Score) error
}

// HTTPLogSink POSTs OTLPEvaluationBatchEnvelope messages to an
// OTLP/HTTP /v1/logs endpoint, batched in-memory with a 200-size
// batch or 2-second flush interval (whichever fires first). The
// the dispatcher's OTLP envelope is the unit shape collectors
// already understand; we batch to amortise the per-event POST
// overhead.
type HTTPLogSink struct {
	log      *slog.Logger
	endpoint string
	client   *http.Client
	ch       chan Score
	done     chan struct{}
	closed   chan struct{}
	wg       sync.WaitGroup

	mu      sync.Mutex
	batch   []Score
	flushed int
}

// NewHTTPLogSink constructs the sink and starts the flusher
// goroutine. If endpoint is empty the sink is functional but
// discard-mode: each emit returns nil without doing any work.
func NewHTTPLogSink(endpoint string, client *http.Client, lg *slog.Logger) *HTTPLogSink {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if lg == nil {
		lg = slog.Default()
	}
	s := &HTTPLogSink{
		log:      lg,
		endpoint: endpoint,
		client:   client,
		ch:       make(chan Score, 1024),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
		batch:    make([]Score, 0, 200),
	}
	s.wg.Add(1)
	go s.flushLoop()
	return s
}

// EmitShip enqueues an event for the next flush. Non-blocking: if
// the buffered channel is full we drop the event (and log it).
// Dropping is the same trade-off OTLPRecorder makes for traces.
func (s *HTTPLogSink) EmitShip(_ context.Context, _ string, score Score) error {
	if s.endpoint == "" {
		return nil
	}
	select {
	case s.ch <- score:
		return nil
	case <-s.done:
		return nil
	default:
		s.log.Warn("otlp evaluation log sink queue full; dropping score",
			"metric", score.Metric, "evaluator", score.Evaluator, "trace_id", score.TraceID)
		return nil
	}
}

// Close drains the buffered channel and exits the flusher. After
// Close, EmitShip is a no-op.
func (s *HTTPLogSink) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
	}
	close(s.done)
	s.wg.Wait()
	close(s.closed)
	return nil
}

// LogsFlushed is exposed so metrics can report cumulative count.
// Returned as int for symmetry with OTLPRecorder; production
// metrics read it through the metrics-recorder hook.
func (s *HTTPLogSink) LogsFlushed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushed
}

func (s *HTTPLogSink) flushLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			for {
				select {
				case sc := <-s.ch:
					s.append(sc)
				default:
					s.flushNow()
					return
				}
			}
		case sc := <-s.ch:
			s.append(sc)
			if len(s.batch) >= 200 {
				s.flushNow()
			}
		case <-ticker.C:
			if len(s.batch) > 0 {
				s.flushNow()
			}
		}
	}
}

func (s *HTTPLogSink) append(sc Score) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batch = append(s.batch, sc)
}

func (s *HTTPLogSink) flushNow() {
	s.mu.Lock()
	batch := s.batch
	s.batch = make([]Score, 0, 200)
	s.mu.Unlock()

	if len(batch) == 0 || s.endpoint == "" {
		return
	}

	body, err := json.Marshal(OTLPEvaluationBatchEnvelope(batch))
	if err != nil {
		s.log.Warn("otlp evaluation log sink: marshal failed", "err", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		s.log.Warn("otlp evaluation log sink: prepare failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		s.log.Warn("otlp evaluation log sink: send failed", "err", err, "count", len(batch))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		s.log.Warn("otlp evaluation log sink: non-2xx",
			"status", resp.StatusCode, "body", string(snippet), "count", len(batch))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	s.mu.Lock()
	s.flushed += len(batch)
	s.mu.Unlock()
}

// NoopLogSink discards every event. The Collector wires this in
// when the OTLP logs endpoint isn't configured.
type NoopLogSink struct{}

// EmitShip discards the event.
func (NoopLogSink) EmitShip(_ context.Context, _ string, _ Score) error { return nil }

// FormatEndpointForLogs adapts the standard OTLP-trace endpoint
// (`http://collector:4318/v1/traces`) to the matching logs route
// (`http://collector:4318/v1/logs`). Exported so main.go can
// derive both endpoints from one config entry.
func FormatEndpointForLogs(traceEndpoint string) string {
	if traceEndpoint == "" {
		return ""
	}
	trimmed := traceEndpoint
	if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '/' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return fmt.Sprintf("%s/v1/logs", trimmed)
}

// Compile-time assertions.
var (
	_ OTLPEvaluationLogSink = (*HTTPLogSink)(nil)
	_ OTLPEvaluationLogSink = NoopLogSink{}
)
