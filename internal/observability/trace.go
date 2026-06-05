// Package observability records LLM request traces following the OpenTelemetry
// GenAI semantic conventions (gen_ai.* attributes) and persists them for
// analysis. Recording is fire-and-forget so it never blocks the request path.
package observability

import (
	"context"
	"time"
)

// Trace is a single gateway request/response record. Field names mirror the
// OpenTelemetry GenAI semantic conventions so traces can be exported to any
// OTLP-compatible backend without remapping.
type Trace struct {
	TraceID   string    `json:"trace_id"`
	SpanID    string    `json:"span_id"`
	ParentID  string    `json:"parent_span_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	// Tenancy / auth context.
	OrgID        string `json:"org_id,omitempty"`
	VirtualKeyID string `json:"virtual_key_id,omitempty"`

	// gen_ai.* attributes.
	OperationName string  `json:"gen_ai.operation.name"` // e.g. "chat"
	ProviderName  string  `json:"gen_ai.provider.name"`  // e.g. "openai"
	RequestModel  string  `json:"gen_ai.request.model"`  // requested model id
	ResponseModel string  `json:"gen_ai.response.model"` // model that actually served
	InputTokens   int     `json:"gen_ai.usage.input_tokens"`
	OutputTokens  int     `json:"gen_ai.usage.output_tokens"`
	FinishReason  string  `json:"gen_ai.response.finish_reasons"`
	Temperature   float64 `json:"gen_ai.request.temperature"`
	TopP          float64 `json:"gen_ai.request.top_p"`
	MaxTokens     int     `json:"gen_ai.request.max_tokens"`

	// Performance.
	Streamed   bool    `json:"streamed"`
	TTFTMillis int64   `json:"ttft_ms"`    // time to first token
	LatencyMs  int64   `json:"latency_ms"` // total wall time
	CostUSD    float64 `json:"cost_usd"`   // computed from usage (0 if unknown)

	// Outcome.
	StatusCode int    `json:"status_code"`
	ErrorType  string `json:"error_type,omitempty"`
	ErrorMsg   string `json:"error_message,omitempty"`

	// GuardrailAction records an inline guardrail decision (e.g. "input_blocked",
	// "output_redacted"). Surfaced on the live trace feed; not persisted to the
	// ClickHouse trace table.
	GuardrailAction string `json:"guardrail_action,omitempty"`

	// Captured content (opt-in; may be empty when content capture is disabled).
	InputMessages  string `json:"gen_ai.input.messages,omitempty"`
	OutputMessages string `json:"gen_ai.output.messages,omitempty"`

	// RAG eval inputs (from client nexus_eval block). JSON array of context
	// strings; persisted for async eval and trace replay.
	RetrievalContexts string `json:"nexus.eval.contexts,omitempty"`
	EvalReference     string `json:"nexus.eval.reference,omitempty"`

	// CacheHit marks a response served from the semantic cache (no upstream call).
	CacheHit bool `json:"cache_hit,omitempty"`
}

// Recorder persists traces. Implementations must be non-blocking from the
// caller's perspective and safe for concurrent use.
type Recorder interface {
	// Record enqueues a trace for persistence. It must not block on I/O.
	Record(t Trace)
	// Close flushes any buffered traces.
	Close(ctx context.Context) error
}

// NoopRecorder discards all traces. Used when no trace store is configured.
type NoopRecorder struct{}

// Record implements Recorder.
func (NoopRecorder) Record(Trace) {}

// Close implements Recorder.
func (NoopRecorder) Close(context.Context) error { return nil }
