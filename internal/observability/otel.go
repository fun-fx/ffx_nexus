package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// OTLPRecorder fans a Trace out as a JSON POST to an OTLP/HTTP endpoint
// (the receiver route that proxy-aware OpenTelemetry Collectors expose,
// e.g. `http://otel-collector:4318/v1/traces`). It is intentionally a thin
// adapter: the collector does the heavy lifting of turning the envelope
// into Prometheus remote-write, Tempo, Honeycomb, Jaeger, etc.
//
// Failures are loud (logged) but never block the gateway — observability
// adapters run in the hot path asynchronously, same as the CHRecorder and
// MetricsRecorder. Disable by leaving NEXUS_OTLP_ENDPOINT empty.
type OTLPRecorder struct {
	log      *slog.Logger
	endpoint string
	client   *http.Client
	ch       chan Trace
	done     chan struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
}

// OTLPOptions configures the OTLP adapter (HTTP only — gRPC is out of
// scope for V3; OTel operators typically expose proxy.otlptracegrpc on a
// different port not worth opening here).
type OTLPOptions struct {
	Endpoint   string        // full URL like http://otel-collector:4318/v1/traces
	BatchSize  int           // default 200
	FlushEvery time.Duration // default 2s
	BufferSize int           // default 10000
	Timeout    time.Duration // default 5s per batch send
}

// NewOTLPRecorder returns nil if Endpoint is empty (callers should treat
// nil as "disabled" and not append to MultiRecorder). Returns a running
// recorder otherwise.
func NewOTLPRecorder(opts OTLPOptions, log *slog.Logger) *OTLPRecorder {
	if opts.Endpoint == "" {
		return nil
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 200
	}
	if opts.FlushEvery == 0 {
		opts.FlushEvery = 2 * time.Second
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = 10000
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	rec := &OTLPRecorder{
		log:      log,
		endpoint: opts.Endpoint,
		client: &http.Client{
			Timeout: opts.Timeout,
		},
		ch:     make(chan Trace, opts.BufferSize),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	rec.wg.Add(1)
	go rec.loop(opts.BatchSize, opts.FlushEvery)
	return rec
}

// Record enqueues a Trace; never blocks (a full buffer drops the trace,
// matching the ClickHouse / Prometheus semantics).
func (r *OTLPRecorder) Record(t Trace) {
	if r == nil {
		return
	}
	select {
	case r.ch <- t:
	default:
		r.log.Warn("otlp buffer full, dropping trace", "trace_id", t.TraceID)
	}
}

func (r *OTLPRecorder) loop(batchSize int, flushEvery time.Duration) {
	defer r.wg.Done()
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	buf := make([]Trace, 0, batchSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.send(buf); err != nil {
			r.log.Warn("otlp export failed", "err", err, "count", len(buf))
		}
		buf = buf[:0]
	}

	for {
		select {
		case <-r.done:
			for {
				select {
				case t := <-r.ch:
					buf = append(buf, t)
					if len(buf) >= batchSize {
						flush()
					}
				default:
					flush()
					close(r.closed)
					return
				}
			}
		case t := <-r.ch:
			buf = append(buf, t)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// send batches the traces into a single HTTP POST. We send a slim envelope
// (just the per-trace key/value tuples) so the collector can map it into
// OTLP resources/spans without needing the protobuf types in-process.
// Operators who need strict OTLP/protobuf can run our collector alongside
// a transformation pipeline (otel-collector-config defined in
// deploy/observability/otel-collector-config.yml does exactly that).
func (r *OTLPRecorder) send(traces []Trace) error {
	payload, err := json.Marshal(traces)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errUnexpectedStatus(resp.StatusCode)
	}
	return nil
}

// Close drains the buffer and stops the background flusher.
func (r *OTLPRecorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	close(r.done)
	select {
	case <-r.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unexpectedStatusError is a sentinel for non-2xx responses without
// importing errors everywhere else just for one helper.
type unexpectedStatusError struct{ code int }

func (e *unexpectedStatusError) Error() string {
	return "otlp unexpected status code " + intToString(e.code)
}

func errUnexpectedStatus(code int) error { return &unexpectedStatusError{code: code} }

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
