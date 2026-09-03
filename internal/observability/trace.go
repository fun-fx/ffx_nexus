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
	UserID       string `json:"user_id,omitempty"`
	VirtualKeyID string `json:"virtual_key_id,omitempty"`

	// CredentialSource records which upstream key served the request:
	// "env"/"org"/"user" (BYOK) or "none" when a strict-BYOK key was missing.
	CredentialSource string `json:"credential_source,omitempty"`

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

	// Customer content. Always populated on the in-memory Trace, because
	// the evaluators cannot score a request whose bodies are blank, and
	// stripped by CaptureGate before any recorder that retains sees it
	// unless NEXUS_CAPTURE_TRACE_CONTENT is set. So "is this populated"
	// depends on which side of the fan-out is asking, and a recorder that
	// starts persisting these must be placed behind the gate in
	// cmd/nexus's traceFanout. See capture.go.
	InputMessages  string `json:"gen_ai.input.messages,omitempty"`
	OutputMessages string `json:"gen_ai.output.messages,omitempty"`

	// RetrievalContexts traces for async eval and trace replay. JSON
	// array of context strings.
	RetrievalContexts string `json:"nexus.eval.contexts,omitempty"`
	EvalReference     string `json:"nexus.eval.reference,omitempty"`

	// EvalMetadata is a vendor- or trace-level bag of metadata. The
	// in-process heuristic metrics read it through `references_from`
	// args — see internal/evaluators/heuristic/local.go. Plugins
	// can also write EvalMetadata directly when fanning out a
	// trace.
	EvalMetadata map[string]any `json:"nexus.eval.metadata,omitempty"`

	// Capture origin info for multi-node deployments. ReplicaID is set once
	// at gateway startup from NEXUS_REPLICA_ID (or the pod hostname / a random
	// suffix) so traces can be bucketed by `gateway_traces GROUP BY replica_id`.
	// It is the operator's debugging handle when scaling out behind an LB.
	ReplicaID string `json:"replica_id,omitempty"`

	// CacheHit marks a response served from the semantic cache (no upstream call).
	CacheHit bool `json:"cache_hit,omitempty"`

	// SessionID is the stable per-conversation marker the gateway
	// extracts from incoming request metadata when the client sets one
	// (Cursor agent: metadata.session_id or sessionId, OpenAI Responses:
	// metadata.conversation_id). Empty when the wire did not carry a
	// marker, and a stable sentinel the frontend can fold on roll-up.
	// Used by /api/stats/overview to merge N consecutive traces from
	// the same conversation (Cursor agent loop, multi-tool runs) into
	// a single session row.
	SessionID string `json:"session_id,omitempty"`

	// TurnID groups the N model calls an agent makes while answering one
	// user question. Unlike SessionID it is derived by the gateway rather
	// than read off the wire — see deriveTurnKey in internal/gateway —
	// because no client we serve sends a correlating marker today. Empty
	// when the request carried no user message to key on, in which case
	// the console falls back to showing the trace on its own row.
	TurnID string `json:"turn_id,omitempty"`
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
