package evals

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// OTLPEvaluationEvent renders a Score as an OTLP/JSON span carrying
// a single `gen_ai.evaluation.result` event. The wire shape follows
// the OTel GenAI semantic conventions:
//
//	{
//	  "traceId":         "<32-hex>",
//	  "spanId":          "<16-hex>",
//	  "name":            "gen_ai.evaluation",
//	  "events": [{
//	    "timeUnixNano":  "<ns>",
//	    "name":          "gen_ai.evaluation.result",
//	    "attributes": [
//	      "gen_ai.evaluation.score.name",
//	      "gen_ai.evaluation.score.value",
//	      "gen_ai.evaluation.score.label",
//	      "gen_ai.evaluation.explanation",
//	      "gen_ai.evaluation.evaluator",
//	    ]
//	  }]
//	}
//
// Returning a span envelope (rather than a logRecords envelope) is
// deliberate: many OTLP collectors handle span events like ordinary
// span attributes — Confident AI, Datadog, Arize Phoenix, Azure AI
// Foundry — so emitting as span events makes the event visible to
// the largest subset of those collectors without per-vendor code.
//
// The label derived from score.Passed follows CloudEvents-style
// "pass"/"fail" naming; pair collectors that key on
// "gen_ai.evaluation.score.label" will see those literal values.
func OTLPEvaluationEvent(traceID string, score Score) map[string]any {
	spanID := hexSpanID_OTLP(stripDashes_OTLP(score.TraceID))
	if spanID == "" || spanID == "0000000000000000" {
		// Use the trace_id for the span_id so parent/child still
		// correlate in collectors that surface a 1:1 event-tree.
		spanID = hexSpanID_OTLP(stripDashes_OTLP(traceID))
	}
	label := "fail"
	if score.Passed {
		label = "pass"
	}
	attrs := filterNilOTLP([]map[string]any{
		kvOTLP("gen_ai.evaluation.score.name", score.Metric),
		kvOTLP("gen_ai.evaluation.score.value", floatToStringOTLP(score.Score)),
		kvOTLP("gen_ai.evaluation.score.label", label),
		kvOTLP("gen_ai.evaluation.explanation", score.Rationale),
		kvOTLP("gen_ai.evaluation.evaluator", score.Evaluator),
	})
	spanTrace := score.TraceID
	if spanTrace == "" {
		spanTrace = traceID
	}
	now := time.Now().UnixNano()
	return map[string]any{
		"name": "gen_ai.evaluation",
		// lowerCamelCase per the protobuf JSON mapping. The rest of this
		// envelope was already camelCase; these two were not, and a
		// receiver that only matches the spec spelling drops the span
		// while still answering 200.
		"traceId":           stripDashes_OTLP(spanTrace),
		"spanId":            spanID,
		"startTimeUnixNano": now,
		"endTimeUnixNano":   now,
		"events": []map[string]any{
			{
				"timeUnixNano": fmt.Sprintf("%d", now),
				"name":         "gen_ai.evaluation.result",
				"attributes":   attrs,
			},
		},
	}
}

// OTLPEvaluationEventEnvelope wraps the unit span inside the
// standard resourceSpans/scopeSpans skeleton so a single OTLP POST
// can carry both the score span and any trace spans the caller
// already ordered. The resource.attributes advertise service.name =
// "nexus" so collectors route the event into the same dashboard
// stream that the trace exporter already uses.
func OTLPEvaluationEventEnvelope(traceID string, score Score) map[string]any {
	return map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						kvOTLP("service.name", "nexus"),
						kvOTLP("telemetry.sdk.language", "go"),
					},
				},
				"scopeSpans": []map[string]any{
					{
						"scope": map[string]any{"name": "nexus.eval"},
						"spans": []map[string]any{
							OTLPEvaluationEvent(traceID, score),
						},
					},
				},
			},
		},
	}
}

// OTLPEvaluationEventJSON is the convenience wrapper that returns
// the marshalled envelope bytes. Adapters that POST their own
// envelope (without going through OTLPEnvelope) call this directly.
func OTLPEvaluationEventJSON(traceID string, score Score) ([]byte, error) {
	return json.Marshal(OTLPEvaluationEventEnvelope(traceID, score))
}

// OTLPEvaluationBatchEnvelope renders a list of scores as a single
// OTLP/JSON resourceSpans envelope carrying one span per score.
//
// We deliberately use the span envelope shape rather than the
// logRecords shape so the same OTLP receiver parser — the one
// grade libraries' span parsers already use — can also extract
// our evaluation events. The collector does not care about the
// distinction between "we polled traces" and "we polled scores"
// as long as the events have the right `event.name`
// (`gen_ai.evaluation.result`) attribute.
func OTLPEvaluationBatchEnvelope(scores []Score) map[string]any {
	if len(scores) == 0 {
		return map[string]any{"resourceSpans": []map[string]any{}}
	}
	spans := make([]map[string]any, 0, len(scores))
	for _, sc := range scores {
		spans = append(spans, OTLPEvaluationEvent(sc.TraceID, sc))
	}
	return map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						kvOTLP("service.name", "nexus"),
						kvOTLP("telemetry.sdk.language", "go"),
					},
				},
				"scopeSpans": []map[string]any{
					{
						"scope": map[string]any{"name": "nexus.eval"},
						"spans": spans,
					},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Helpers — kept here rather than in internal/observability to avoid
// pulling that package from evals (the reverse direction is fine:
// observability already depends on evals via internal/observability
// imports being absent, but evals already depends on observability —
// a circular import is unavoidable if these helpers live there).
// ---------------------------------------------------------------------------

func kvOTLP(key string, value any) map[string]any {
	if !isNonEmptyOTLP(value) {
		return nil
	}
	return map[string]any{"key": key, "value": map[string]any{otlpValueTypeOTLP(value): value}}
}

func isNonEmptyOTLP(v any) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok && s == "" {
		return false
	}
	return true
}

func otlpValueTypeOTLP(v any) string {
	switch v.(type) {
	case bool:
		return "boolValue"
	case int, int32, int64, float32, float64:
		return "doubleValue"
	case string:
		return "stringValue"
	default:
		// arrays/maps go through JsonValue; we don't emit those
		// here because evals.Score fields are all scalars.
		return "stringValue"
	}
}

func filterNilOTLP(in []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, m := range in {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

// stripDashes_OTLP removes "-" from UUID-shaped ids.
func stripDashes_OTLP(s string) string { return strings.ReplaceAll(s, "-", "") }

// hexSpanID_OTLP trims or pads a string to 16 hex characters. Truthy
// hex digit detection matches what observability/otel.go uses so the
// two helpers stay byte-compatible.
func hexSpanID_OTLP(s string) string {
	if s == "" {
		return ""
	}
	cleaned := strings.Builder{}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			cleaned.WriteRune(r)
		}
	}
	out := cleaned.String()
	if len(out) > 16 {
		return out[len(out)-16:]
	}
	if len(out) < 16 {
		return out + strings.Repeat("0", 16-len(out))
	}
	return out
}

func floatToStringOTLP(f float64) string {
	if f == 0 {
		return "0"
	}
	return fmt.Sprintf("%g", f)
}
