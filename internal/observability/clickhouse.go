package observability

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHRecorder buffers traces and flushes them to ClickHouse in batches. The
// request path only ever does a non-blocking channel send, so a slow or
// unavailable ClickHouse never adds latency to proxied requests.
type CHRecorder struct {
	conn   driver.Conn
	log    *slog.Logger
	ch     chan Trace
	done   chan struct{}
	wg     sync.WaitGroup
	closed chan struct{}

	batchSize int
	flushEach time.Duration
}

// CHOptions configures the recorder.
type CHOptions struct {
	BatchSize  int
	FlushEvery time.Duration
	BufferSize int
}

// NewCHRecorder connects to ClickHouse using a native-protocol DSN and starts
// the background flusher.
func NewCHRecorder(ctx context.Context, dsn string, opts CHOptions, log *slog.Logger) (*CHRecorder, error) {
	chOpts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(chOpts)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, err
	}

	if opts.BatchSize == 0 {
		opts.BatchSize = 500
	}
	if opts.FlushEvery == 0 {
		opts.FlushEvery = 2 * time.Second
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = 10000
	}

	r := &CHRecorder{
		conn:      conn,
		log:       log,
		ch:        make(chan Trace, opts.BufferSize),
		done:      make(chan struct{}),
		closed:    make(chan struct{}),
		batchSize: opts.BatchSize,
		flushEach: opts.FlushEvery,
	}
	r.wg.Add(1)
	go r.loop()
	return r, nil
}

// Record enqueues a trace without blocking. If the buffer is full the trace is
// dropped (observability must never back-pressure the gateway).
func (r *CHRecorder) Record(t Trace) {
	select {
	case r.ch <- t:
	default:
		r.log.Warn("trace buffer full, dropping trace", "trace_id", t.TraceID)
	}
}

func (r *CHRecorder) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.flushEach)
	defer ticker.Stop()

	buf := make([]Trace, 0, r.batchSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.insert(buf); err != nil {
			r.log.Error("clickhouse insert failed", "err", err, "count", len(buf))
		}
		buf = buf[:0]
	}

	for {
		select {
		case <-r.done:
			// Drain remaining buffered traces before exiting.
			for {
				select {
				case t := <-r.ch:
					buf = append(buf, t)
					if len(buf) >= r.batchSize {
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
			if len(buf) >= r.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *CHRecorder) insert(traces []Trace) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	batch, err := r.conn.PrepareBatch(ctx, `INSERT INTO gateway_traces`)
	if err != nil {
		return err
	}
	for _, t := range traces {
		if err := batch.Append(
			t.TraceID, t.SpanID, t.ParentID, t.Timestamp,
			t.OrgID, t.VirtualKeyID,
			t.OperationName, t.ProviderName, t.RequestModel, t.ResponseModel,
			uint32(t.InputTokens), uint32(t.OutputTokens), t.FinishReason,
			t.Temperature, t.TopP, uint32(t.MaxTokens),
			boolToUint8(t.Streamed), t.TTFTMillis, t.LatencyMs, t.CostUSD,
			uint16(t.StatusCode), t.ErrorType, t.ErrorMsg,
			t.InputMessages, t.OutputMessages,
			t.RetrievalContexts, t.EvalReference,
			boolToUint8(t.CacheHit), t.GuardrailAction,
			t.UserID, t.CredentialSource,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// Conn exposes the underlying ClickHouse connection so sibling subsystems
// (e.g. the eval worker's score sink) can reuse the same pool.
func (r *CHRecorder) Conn() driver.Conn { return r.conn }

// Migrate applies a SQL script (semicolon-separated statements) to ClickHouse.
// Used to create the trace/eval tables on startup.
func (r *CHRecorder) Migrate(ctx context.Context, script string) error {
	for _, stmt := range strings.Split(script, ";") {
		if !containsSQL(stmt) {
			continue
		}
		if err := r.conn.Exec(ctx, strings.TrimSpace(stmt)); err != nil {
			return err
		}
	}
	return nil
}

// containsSQL reports whether a statement fragment has any non-comment content.
func containsSQL(stmt string) bool {
	for _, line := range strings.Split(stmt, "\n") {
		l := strings.TrimSpace(line)
		if l != "" && !strings.HasPrefix(l, "--") {
			return true
		}
	}
	return false
}

// Close stops the flusher and waits for buffered traces to be written.
func (r *CHRecorder) Close(ctx context.Context) error {
	close(r.done)
	select {
	case <-r.closed:
	case <-ctx.Done():
	}
	r.wg.Wait()
	return r.conn.Close()
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
