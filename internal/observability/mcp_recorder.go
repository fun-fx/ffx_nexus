package observability

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHMCPLogRecorder buffers MCP logs and flushes to ClickHouse.
type CHMCPLogRecorder struct {
	conn      driver.Conn
	log       *slog.Logger
	ch        chan MCPLog
	done      chan struct{}
	wg        sync.WaitGroup
	closed    chan struct{}
	batchSize int
	flushEach time.Duration
}

// CHMCPOptions configures the MCP log recorder.
type CHMCPOptions struct {
	BatchSize  int
	FlushEvery time.Duration
	BufferSize int
}

// NewCHMCPLogRecorder starts a background flusher on an existing connection.
func NewCHMCPLogRecorder(conn driver.Conn, opts CHMCPOptions, log *slog.Logger) *CHMCPLogRecorder {
	if opts.BatchSize == 0 {
		opts.BatchSize = 500
	}
	if opts.FlushEvery == 0 {
		opts.FlushEvery = 2 * time.Second
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = 10000
	}
	r := &CHMCPLogRecorder{
		conn:      conn,
		log:       log,
		ch:        make(chan MCPLog, opts.BufferSize),
		done:      make(chan struct{}),
		closed:    make(chan struct{}),
		batchSize: opts.BatchSize,
		flushEach: opts.FlushEvery,
	}
	r.wg.Add(1)
	go r.loop()
	return r
}

// Record enqueues without blocking.
func (r *CHMCPLogRecorder) Record(l MCPLog) {
	select {
	case r.ch <- l:
	default:
		r.log.Warn("mcp log buffer full, dropping", "id", l.ID)
	}
}

func (r *CHMCPLogRecorder) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.flushEach)
	defer ticker.Stop()
	buf := make([]MCPLog, 0, r.batchSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.insert(buf); err != nil {
			r.log.Error("clickhouse mcp insert failed", "err", err, "count", len(buf))
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-r.done:
			for {
				select {
				case l := <-r.ch:
					buf = append(buf, l)
					if len(buf) >= r.batchSize {
						flush()
					}
				default:
					flush()
					close(r.closed)
					return
				}
			}
		case l := <-r.ch:
			buf = append(buf, l)
			if len(buf) >= r.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *CHMCPLogRecorder) insert(logs []MCPLog) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	batch, err := r.conn.PrepareBatch(ctx, `INSERT INTO mcp_tool_logs (
		id, timestamp, org_id, user_id, virtual_key_id,
		server_id, server_label, tool_name, status,
		latency_ms, cost_usd, llm_trace_id, session_id, turn_id, request_id,
		arguments, result, error_message, metadata)`)
	if err != nil {
		return err
	}
	for _, l := range logs {
		if err := batch.Append(
			l.ID, l.Timestamp, l.OrgID, l.UserID, l.VirtualKeyID,
			l.ServerID, l.ServerLabel, l.ToolName, l.Status,
			l.LatencyMs, l.CostUSD, l.LLMTraceID, l.SessionID, l.TurnID, l.RequestID,
			l.Arguments, l.Result, l.ErrorMessage, l.Metadata,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// Close stops the flusher.
func (r *CHMCPLogRecorder) Close(ctx context.Context) error {
	close(r.done)
	select {
	case <-r.closed:
	case <-ctx.Done():
	}
	r.wg.Wait()
	return nil
}

// MultiMCPLogRecorder fans out to several MCP log recorders.
type MultiMCPLogRecorder struct {
	recorders []MCPLogRecorder
}

// NewMultiMCPLogRecorder composes MCP log recorders.
func NewMultiMCPLogRecorder(recorders ...MCPLogRecorder) *MultiMCPLogRecorder {
	out := make([]MCPLogRecorder, 0, len(recorders))
	for _, r := range recorders {
		if r != nil {
			out = append(out, r)
		}
	}
	return &MultiMCPLogRecorder{recorders: out}
}

// Record forwards to all recorders.
func (m *MultiMCPLogRecorder) Record(l MCPLog) {
	for _, r := range m.recorders {
		r.Record(l)
	}
}

// Close closes all recorders.
func (m *MultiMCPLogRecorder) Close(ctx context.Context) error {
	var first error
	for _, r := range m.recorders {
		if err := r.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// MCPLogCaptureGate strips payloads when capture is disabled.
type MCPLogCaptureGate struct {
	inner   MCPLogRecorder
	capture bool
}

// NewMCPLogCaptureGate wraps a recorder with optional payload stripping.
func NewMCPLogCaptureGate(inner MCPLogRecorder, capture bool) *MCPLogCaptureGate {
	return &MCPLogCaptureGate{inner: inner, capture: capture}
}

// Record implements MCPLogRecorder.
func (g *MCPLogCaptureGate) Record(l MCPLog) {
	if !g.capture {
		l.Arguments = ""
		l.Result = ""
	}
	if g.inner != nil {
		g.inner.Record(l)
	}
}

// Close implements MCPLogRecorder.
func (g *MCPLogCaptureGate) Close(ctx context.Context) error {
	if g.inner == nil {
		return nil
	}
	return g.inner.Close(ctx)
}
