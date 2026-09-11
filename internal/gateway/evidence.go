package gateway

import (
	"net/http"

	"github.com/ffxnexus/nexus/internal/observability"
)

// SetEgressMode stamps the chart-rendered provider-egress contract on traces.
func (h *Handler) SetEgressMode(mode string) {
	if h != nil {
		h.egressMode = mode
	}
}

func (h *Handler) recordEvidence(t observability.Trace, attempts []observability.ProviderAttempt, reasons []observability.PolicyReason) {
	if h == nil || h.recorder == nil {
		return
	}
	t.Attempts = append([]observability.ProviderAttempt(nil), attempts...)
	t.PolicyReasons = append([]observability.PolicyReason(nil), reasons...)
	t.EgressMode = h.egressMode
	if t.GuardrailRule == "" {
		t.GuardrailRule = observability.GuardrailRuleFromAction(t.GuardrailAction)
	}
	h.recorder.Record(t)
}

func (h *Handler) recordPolicyDenied(r *http.Request, req ChatCompletionRequest, status int, code, detail string) {
	t := h.newTrace(r, req, "")
	t.StatusCode = status
	t.ErrorType = code
	t.ErrorMsg = detail
	h.recordEvidence(t, nil, []observability.PolicyReason{{Code: code, Detail: detail}})
}

func attemptFromTrace(t observability.Trace, index int, chainLen int) observability.ProviderAttempt {
	return observability.ProviderAttempt{
		Index:            index,
		Provider:         t.ProviderName,
		Model:            t.RequestModel,
		CredentialSource: t.CredentialSource,
		StatusCode:       t.StatusCode,
		LatencyMs:        t.LatencyMs,
		ErrorType:        t.ErrorType,
		ErrorMsg:         t.ErrorMsg,
		CacheHit:         t.CacheHit,
		FallbackAllowed:  index < chainLen-1,
	}
}

func stampSharedTraceID(dst *observability.Trace, reqTrace observability.Trace) {
	dst.TraceID = reqTrace.TraceID
	if dst.ParentID == "" {
		dst.ParentID = reqTrace.ParentID
	}
	if dst.SessionID == "" {
		dst.SessionID = reqTrace.SessionID
	}
	if dst.TurnID == "" {
		dst.TurnID = reqTrace.TurnID
	}
}
