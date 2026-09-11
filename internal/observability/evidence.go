package observability

import (
	"encoding/json"
	"strings"
)

// Typed policy decision codes persisted on a Trace and shown in the
// request evidence graph. Keep these stable: they are the export contract.
const (
	ReasonAllowed           = "allowed"
	ReasonDenied            = "denied"
	ReasonFallback          = "fallback"
	ReasonRedacted          = "redacted"
	ReasonMinQuality        = "min_quality"
	ReasonCacheHit          = "cache_hit"
	ReasonMissingBYOK       = "missing_byok_key"
	ReasonGuardrailBlocked  = "guardrail_blocked"
	ReasonModelNotAllowed   = "model_not_allowed"
	ReasonSchemaBlocked     = "schema_blocked"
	ReasonUpstreamError     = "upstream_error"
)

// ProviderAttempt is one hop in a fallback chain.
type ProviderAttempt struct {
	Index            int    `json:"index"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	CredentialSource string `json:"credential_source,omitempty"`
	StatusCode       int    `json:"status_code"`
	LatencyMs        int64  `json:"latency_ms"`
	ErrorType        string `json:"error_type,omitempty"`
	ErrorMsg         string `json:"error_message,omitempty"`
	CacheHit         bool   `json:"cache_hit,omitempty"`
	FallbackAllowed  bool   `json:"fallback_allowed"`
}

// PolicyReason is a typed, rule-identified decision.
type PolicyReason struct {
	Code   string `json:"code"`
	RuleID string `json:"rule_id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// EvalScoreRow is one eval_scores join against a trace.
type EvalScoreRow struct {
	Evaluator  string  `json:"evaluator"`
	Metric     string  `json:"metric"`
	Score      float64 `json:"score"`
	Passed     bool    `json:"passed"`
	Rationale  string  `json:"rationale,omitempty"`
	JudgeModel string  `json:"judge_model,omitempty"`
}

// TraceEvidence is the request graph export: list metadata plus attempt
// trail, policy reasons, and async eval scores.
type TraceEvidence struct {
	TraceSummary
	Attempts      []ProviderAttempt `json:"attempts"`
	PolicyReasons []PolicyReason    `json:"policy_reasons"`
	GuardrailRule string            `json:"guardrail_rule,omitempty"`
	EgressMode    string            `json:"egress_mode,omitempty"`
	EvalScores    []EvalScoreRow    `json:"eval_scores"`
}

// MarshalJSONField encodes v as a JSON string for ClickHouse String columns.
// Empty slices and nil become "".
func MarshalJSONField(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	s := string(b)
	if s == "null" || s == "[]" {
		return ""
	}
	return s
}

// UnmarshalJSONField decodes a ClickHouse JSON string into dst. Empty is a no-op.
func UnmarshalJSONField(raw string, dst any) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return
	}
	_ = json.Unmarshal([]byte(raw), dst)
}

// GuardrailRuleFromAction pulls the rule id off "input_blocked:pii_input".
func GuardrailRuleFromAction(action string) string {
	_, rule, ok := strings.Cut(action, ":")
	if !ok {
		return ""
	}
	return rule
}
