package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
)

// MCPOtlPLogRecorder exports MCP tool spans to an OTLP/HTTP endpoint.
type MCPOtlPLogRecorder struct {
	log      *slog.Logger
	endpoint string
	client   *http.Client
	ch       chan MCPLog
	done     chan struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
}

// NewMCPOtlPLogRecorder returns nil when endpoint is empty.
func NewMCPOtlPLogRecorder(endpoint string, timeout time.Duration, log *slog.Logger) *MCPOtlPLogRecorder {
	if endpoint == "" {
		return nil
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	rec := &MCPOtlPLogRecorder{
		log:      log,
		endpoint: endpoint,
		client:   egress.Client(egress.Operator, timeout),
		ch:       make(chan MCPLog, 10000),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
	}
	rec.wg.Add(1)
	go rec.loop()
	return rec
}

func (r *MCPOtlPLogRecorder) Record(l MCPLog) {
	if r == nil {
		return
	}
	select {
	case r.ch <- l:
	default:
		r.log.Warn("mcp otlp buffer full, dropping", "id", l.ID)
	}
}

func (r *MCPOtlPLogRecorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	close(r.done)
	select {
	case <-r.closed:
	case <-ctx.Done():
	}
	r.wg.Wait()
	return nil
}

func (r *MCPOtlPLogRecorder) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	buf := make([]MCPLog, 0, 200)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.send(buf); err != nil {
			r.log.Error("mcp otlp export failed", "err", err, "count", len(buf))
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
					if len(buf) >= 200 {
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
			if len(buf) >= 200 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (r *MCPOtlPLogRecorder) send(logs []MCPLog) error {
	spans := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		end := l.Timestamp
		start := end.Add(-time.Duration(l.LatencyMs) * time.Millisecond)
		attrs := []map[string]any{
			{"key": "gen_ai.tool.name", "value": map[string]any{"stringValue": l.ToolName}},
			{"key": "gen_ai.operation.name", "value": map[string]any{"stringValue": "tool_call"}},
			{"key": "nexus.mcp.server", "value": map[string]any{"stringValue": l.ServerLabel}},
			{"key": "nexus.mcp.status", "value": map[string]any{"stringValue": l.Status}},
		}
		if l.LLMTraceID != "" {
			attrs = append(attrs, map[string]any{"key": "nexus.llm_trace_id", "value": map[string]any{"stringValue": l.LLMTraceID}})
		}
		span := map[string]any{
			"traceId":           l.LLMTraceID,
			"spanId":            l.ID,
			"name":              "mcp.tools/call",
			"kind":              1,
			"startTimeUnixNano": strconv.FormatInt(start.UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(end.UnixNano(), 10),
			"attributes":        attrs,
		}
		if l.Status == "error" {
			span["status"] = map[string]any{"code": 2, "message": l.ErrorMessage}
		}
		spans = append(spans, span)
	}
	body := map[string]any{
		"resourceSpans": []map[string]any{{
			"scopeSpans": []map[string]any{{
				"spans": spans,
			}},
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(raw))
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
		return fmt.Errorf("otlp http %d", resp.StatusCode)
	}
	return nil
}
