package observability

import (
	"context"
	"time"
)

// MCPLog is one MCP tool execution record.
type MCPLog struct {
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	OrgID        string    `json:"org_id"`
	UserID       string    `json:"user_id"`
	VirtualKeyID string    `json:"virtual_key_id"`
	ServerID     string    `json:"server_id"`
	ServerLabel  string    `json:"server_label"`
	ToolName     string    `json:"tool_name"`
	Status       string    `json:"status"`
	LatencyMs    int64     `json:"latency_ms"`
	CostUSD      float64   `json:"cost_usd"`
	LLMTraceID   string    `json:"llm_trace_id,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	TurnID       string    `json:"turn_id,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	Arguments    string    `json:"arguments,omitempty"`
	Result       string    `json:"result,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	Metadata     string    `json:"metadata,omitempty"`
}

// MCPLogRecorder persists MCP tool logs without blocking callers.
type MCPLogRecorder interface {
	Record(MCPLog)
	Close(ctx context.Context) error
}

// MCPLogNoop discards MCP logs.
type MCPLogNoop struct{}

func (MCPLogNoop) Record(MCPLog)               {}
func (MCPLogNoop) Close(context.Context) error { return nil }
