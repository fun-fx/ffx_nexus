// Package mcp implements MCP server registry and tool proxying for the gateway.
package mcp

import "time"

// ServerRecord is a persisted MCP server row.
type ServerRecord struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	SpecYAML  string    `json:"spec_yaml"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ServerStatus is the runtime view returned to the console and gateway.
type ServerStatus struct {
	ServerRecord
	ConnectionType string `json:"connection_type"`
	State          string `json:"state"` // healthy | error | disabled | connecting
	ToolCount      int    `json:"tool_count"`
	Tools          []Tool `json:"tools,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// Tool is a discovered MCP tool definition.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CallContext carries tenancy and correlation fields for a tool invocation.
type CallContext struct {
	OrgID        string
	UserID       string
	VirtualKeyID string
	RequestID    string
	LLMTraceID   string
	SessionID    string
	TurnID       string
}

// CallRequest is the gateway wire shape for tools/call.
type CallRequest struct {
	Name       string         `json:"name"`
	Arguments  map[string]any `json:"arguments"`
	LLMTraceID string         `json:"llm_trace_id,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	TurnID     string         `json:"turn_id,omitempty"`
}

// CallResult is returned to gateway callers.
type CallResult struct {
	Content   any    `json:"content"`
	IsError   bool   `json:"is_error"`
	RawJSON   string `json:"raw_json,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}
