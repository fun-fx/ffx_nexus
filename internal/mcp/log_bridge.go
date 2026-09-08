package mcp

import (
	"github.com/ffxnexus/nexus/internal/observability"
)

// LogBridge adapts observability.MCPLogRecorder to mcp.LogSink.
type LogBridge struct {
	Rec observability.MCPLogRecorder
}

// Record implements LogSink.
func (b LogBridge) Record(log ToolLog) {
	if b.Rec == nil {
		return
	}
	b.Rec.Record(observability.MCPLog{
		ID:           log.ID,
		Timestamp:    log.Timestamp,
		OrgID:        log.OrgID,
		UserID:       log.UserID,
		VirtualKeyID: log.VirtualKeyID,
		ServerID:     log.ServerID,
		ServerLabel:  log.ServerLabel,
		ToolName:     log.ToolName,
		Status:       log.Status,
		LatencyMs:    log.LatencyMs,
		CostUSD:      log.CostUSD,
		LLMTraceID:   log.LLMTraceID,
		SessionID:    log.SessionID,
		TurnID:       log.TurnID,
		RequestID:    log.RequestID,
		Arguments:    log.Arguments,
		Result:       log.Result,
		ErrorMessage: log.ErrorMessage,
		Metadata:     log.Metadata,
	})
}
