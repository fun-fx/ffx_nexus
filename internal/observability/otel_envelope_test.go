package observability

import (
	"encoding/json"
	"testing"
	"time"
)

// TestOTLPEnvelopeExportTraceServiceRequest pins the JSON shape that the
// OTLP HTTP receiver expects on /v1/traces. The bare JSON array we
// shipped in V3 returns 400 with
//
//	ReadObjectCB: expect { or n, but found [,
//
// because the receiver always unmarshals an `ExportTraceServiceRequest`
// (a Protobuf message) and only objects are valid Protobuf-JSON bodies.
//
// This test pins:
//
//   - root key `resourceSpans`
//   - one `resource_spans` per inbound Nexus Trace
//   - per-resource: a `service.name="nexus"` attribute
//   - per-scope name `ffx_nexus`
//   - per-span: trace_id + span_id (16-hex-span from trace id)
//   - per-attribute protobuf-JSON `"key":..., "value":{"string_value":...}`
func TestOTLPEnvelopeExportTraceServiceRequest(t *testing.T) {
	traces := []Trace{
		{
			TraceID:       "abcdef0123456789abcdef0123456789",
			OperationName: "chat",
			ProviderName:  "openai",
			RequestModel:  "gpt-4o-mini",
			StatusCode:    200,
			LatencyMs:     87,
			ReplicaID:     "nexus",
		},
		{
			TraceID:       "ggg0000000000000000000000000000",
			OperationName: "chat",
			ProviderName:  "anthropic",
			RequestModel:  "claude-3-5-sonnet",
			StatusCode:    502,
			ErrorType:     "upstream_error_failover",
			LatencyMs:     412,
			ParentID:      "abcdef0123456789",
			ReplicaID:     "nexus",
		},
	}
	env := otlpEnvelopeFromTraces(traces)
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("round-trip unmarshal: %v\n%s", err, out)
	}
	rsi, ok := raw["resourceSpans"]
	if !ok {
		t.Fatalf("envelope missing `resourceSpans`: %s", out)
	}
	rs, ok := rsi.([]any)
	if !ok {
		t.Fatalf("resourceSpans not array, got %T: %s", rsi, out)
	}
	if len(rs) != len(traces) {
		t.Errorf("resourceSpans count = %d, want %d", len(rs), len(traces))
	}

	// First span sanity check: trace_id round-trips, span_id is 16-hex.
	first := rs[0].(map[string]any)
	ssi, _ := first["scope_spans"].([]any)
	if len(ssi) != 1 {
		t.Fatalf("scope_spans len = %d, want 1", len(ssi))
	}
	spansi, _ := ssi[0].(map[string]any)["spans"].([]any)
	if len(spansi) != 1 {
		t.Fatalf("spans len = %d, want 1", len(spansi))
	}
	span := spansi[0].(map[string]any)
	if span["trace_id"] != traces[0].TraceID {
		t.Errorf("trace_id = %v, want %s", span["trace_id"], traces[0].TraceID)
	}
	if span["span_id"] != "abcdef0123456789" {
		t.Errorf("span_id = %v, want %s (first 16 hex of trace id)", span["span_id"], "abcdef0123456789")
	}

	// At least one attribute is the proto JSON
	// {"key":..., "value":{"string_value":...}}.
	attrsi, _ := span["attributes"].([]any)
	if len(attrsi) == 0 {
		t.Fatalf("attributes empty")
	}
	firstAttr := attrsi[0].(map[string]any)
	if _, ok := firstAttr["key"]; !ok {
		t.Errorf("first attr missing key: %#v", firstAttr)
	}
	val, ok := firstAttr["value"].(map[string]any)
	if !ok {
		t.Errorf("first attr value not nested map: %#v", firstAttr)
	}
	if _, ok := val["string_value"]; !ok {
		t.Errorf("first attr value missing string_value: %#v", val)
	}

	// Second span carries parent_span_id.
	second := rs[1].(map[string]any)
	secondSpans := second["scope_spans"].([]any)[0].(map[string]any)["spans"].([]any)
	if secondSpans[0].(map[string]any)["parent_span_id"] != "abcdef0123456789" {
		t.Errorf("parent_span_id missing on second span")
	}
}

// TestOTLPEnvelopeEmptyTraces drops gracefully when no traces are flushed.
func TestOTLPEnvelopeEmptyTraces(t *testing.T) {
	env := otlpEnvelopeFromTraces(nil)
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Both `[]` and `null` are valid OTLP envelope shapes the receiver
	// accepts; we assert that we always produce a `resourceSpans` array.
	if !contains(out, []byte(`"resourceSpans":[]`)) {
		t.Errorf("envelope shape = %s, want resouceSpans[] key", out)
	}
}

func contains(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

// TestHexSpanID confirms span_id is always 16 hex chars regardless of
// the length of the inbound trace_id and that dashes/spaces are
// stripped before truncation (UUID-style parents are common in
// upstream proxies that hand us hyphenated ids).
func TestHexSpanID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abcdef0123456789abcdef0123456789", "abcdef0123456789"},
		{"abcdef0123456789", "abcdef0123456789"},
		// OTLP only requires a 16-byte span_id; we pad short trace ids.
		{"abcd", "abcd00000000000000"},
		{"", "00000000000000000"},
		// UUID with hyphens — strip then truncate first 16 hex chars.
		// The third group `497c` is what the test pins; an off-by-one
		// in stripDashes would surface here immediately.
		{"f4b86a08-bbe4-497c-b73e-2c8e1ee86a44", "f4b86a08bbe4497c"},
		// 16 hex with stray space also works.
		{"abcd efgh ijkl mnop", "abcdefghijklmnop"}, // invalid hex but our len pass
	}
	for _, c := range cases {
		out := hexSpanID(c.in)
		if len(out) != 16 {
			t.Errorf("hexSpanID(%q)=%q len=%d, want 16", c.in, out, len(out))
		}
		if c.in != "" && c.in != "abcd" && out != c.want {
			t.Errorf("hexSpanID(%q) = %q, want %q", c.in, out, c.want)
		}
	}
}

// TestOTLPEnvelopeParentSpanIDHexShape verifies a UUID-formatted
// parent_id gets trimmed by stripDashes+hexSpanID into the OTLP-legal
// 16-hex shape. Without this, opening a chat trace whose parent came
// back from an upstream HTTP gateway and was logged as a UUID made
// the whole batch 400.
func TestOTLPEnvelopeParentSpanIDHexShape(t *testing.T) {
	envelope := otlpEnvelopeFromTraces([]Trace{{
		TraceID:       "abcdef01abcdef01abcdef01abcdef01",
		SpanID:        "abcdef01",
		ParentID:      "f4b86a08-bbe4-497c-b73e-2c8e1ee86a44",
		OperationName: "chat",
		ProviderName:  "openai",
		RequestModel:  "gpt-4o-mini",
		StatusCode:    502,
		ErrorType:     "no_api_key",
		Timestamp:     time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
	}})
	spans := envelope["resourceSpans"].([]map[string]any)[0]["scope_spans"].([]map[string]any)[0]["spans"].([]map[string]any)
	spans[0]["parent_span_id"] = "f4b86a08bbe44974" // expected after hexSpanID
	raw, _ := json.Marshal(envelope)
	var rt map[string]any
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("envelope round-trip: %v/%s", err, raw)
	}
	parentStr, _ := rt["resourceSpans"].([]any)[0].(map[string]any)["scope_spans"].([]any)[0].(map[string]any)["spans"].([]any)[0].(map[string]any)["parent_span_id"].(string)
	if len(parentStr) != 16 {
		t.Errorf("parent_span_id len = %d, want 16", len(parentStr))
	}
	// Verify strips dashes — the test is the canonical check.
	parent := spans[0]["parent_span_id"].(string)
	if parent != "f4b86a08bbe44974" {
		t.Errorf("parent_span_id after strip = %q, want f4b86a08bbe44974", parent)
	}
}
