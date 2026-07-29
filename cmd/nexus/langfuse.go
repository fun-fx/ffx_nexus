// Langfuse plugin adapter.
//
// Send path — OTLP, not the legacy ingestion API. Langfuse exposes
// `POST /api/public/otel/v1/traces` implementing OTLP/HTTP, and that is
// the endpoint they recommend: `/api/public/ingestion` is deprecated and
// on deployments migrated to Langfuse v4 it *rejects* `trace-create`
// events outright (only score, sdk-log and dataset-run-item events are
// still accepted there). We send OTLP/JSON, which Langfuse supports
// alongside protobuf, reusing the same envelope builder as the
// first-party exporter so the gen_ai.* attribute mapping cannot drift.
//
// Collect path — `GET /api/public/v3/scores`. The v2 endpoint is
// deprecated and returns 404 on Langfuse v4, and v3 nests the trace id
// under `subject`, so this adapter flattens each score into the field
// names the manifest's collect.mapping expects.
//
// Both paths authenticate with HTTP Basic auth: username is the project
// public key, password is the secret key.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
	"github.com/ffxnexus/nexus/internal/observability"
)

const (
	langfuseOTLPPath   = "/api/public/otel/v1/traces"
	langfuseScoresPath = "/api/public/v3/scores"
	// langfuseScorePollLimit caps one polling tick. Langfuse returns
	// newest-first, and the collector caps at 1000 per tick anyway.
	langfuseScorePollLimit = 100
)

func langfuseTransmit(ctx context.Context, tgt external.Target, payload map[string]any) error {
	pub, secret, ok := tgt.Auth.Pair()
	if !ok {
		return &adapterError{
			vendor: "langfuse",
			code:   "auth",
			err: errors.New("needs a public key and a secret key: " +
				"set auth.keyRef to two keys, e.g. keyRef: public_key|secret_key"),
		}
	}
	// Content comes from the rendered payload, never straight off the
	// trace: the payload has been through spec.send.redact, so a plugin
	// configured with `redact: [pii]` keeps that promise here.
	envelope := observability.OTLPEnvelope(
		[]observability.Trace{tgt.Trace},
		langfuseSpanAttributes(payload),
	)
	body, err := json.Marshal(envelope)
	if err != nil {
		return &adapterError{vendor: "langfuse", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		joinEndpoint(tgt.Endpoint, langfuseOTLPPath), bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "langfuse", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-langfuse-ingestion-version", "4")
	req.SetBasicAuth(pub, secret)

	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "langfuse", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// Carry a body snippet: Langfuse answers a rejected OTLP
		// envelope with a terse message that is the only clue as to
		// which attribute it disliked.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &adapterError{
			vendor: "langfuse",
			code:   "status_" + strconv.Itoa(resp.StatusCode),
			err:    fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(snippet)),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// langfuseSpanAttributes maps the plugin's rendered payload onto span
// attributes. Keys already written in dotted form are passed through so
// an operator can target a semconv attribute directly; the two
// conventional short names are promoted to their GenAI semconv
// equivalents (which is what makes Langfuse render input/output and run
// its evaluators); anything else is namespaced so it cannot collide
// with a reserved attribute.
func langfuseSpanAttributes(payload map[string]any) map[string]string {
	if len(payload) == 0 {
		return nil
	}
	out := make(map[string]string, len(payload))
	for k, v := range payload {
		s := stringifyPayloadValue(v)
		if s == "" {
			continue
		}
		switch {
		case containsDot(k):
			out[k] = s
		case k == "input":
			out["gen_ai.input.messages"] = s
		case k == "output":
			out["gen_ai.output.messages"] = s
		default:
			out["nexus.plugin."+k] = s
		}
	}
	return out
}

func containsDot(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return true
		}
	}
	return false
}

// stringifyPayloadValue renders a payload value as a span attribute.
// Templates yield strings; anything structured is JSON-encoded so it
// survives as one attribute rather than being dropped.
func stringifyPayloadValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// langfuseFetch reads recent scores so poll-mode plugins can pull
// Langfuse-computed evaluations back into Nexus.
func langfuseFetch(ctx context.Context, tgt external.Target) ([]json.RawMessage, error) {
	pub, secret, ok := tgt.Auth.Pair()
	if !ok {
		return nil, &adapterError{
			vendor: "langfuse",
			code:   "auth",
			err:    errors.New("needs a public key and a secret key in auth.keyRef"),
		}
	}
	// `details` adds comment, `subject` adds the trace id — both are
	// opt-in in v3 and both are needed to build a Nexus score.
	q := url.Values{}
	q.Set("fields", "details,subject")
	q.Set("limit", strconv.Itoa(langfuseScorePollLimit))
	endpoint := joinEndpoint(tgt.Endpoint, langfuseScoresPath) + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &adapterError{vendor: "langfuse", code: "prepare", err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(pub, secret)

	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return nil, &adapterError{vendor: "langfuse", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &adapterError{
			vendor: "langfuse",
			code:   "status_" + strconv.Itoa(resp.StatusCode),
			err:    fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(snippet)),
		}
	}
	var page struct {
		Data []langfuseScore `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, &adapterError{vendor: "langfuse", code: "decode", err: err}
	}
	return flattenLangfuseScores(page.Data), nil
}

// langfuseScore is the subset of a v3 score row Nexus consumes. `value`
// is polymorphic in v3 (number, bool, or string depending on dataType),
// so it stays a RawMessage until flattening.
type langfuseScore struct {
	Name     string          `json:"name"`
	Value    json.RawMessage `json:"value"`
	DataType string          `json:"dataType"`
	Comment  string          `json:"comment"`
	Subject  struct {
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		TraceID string `json:"traceId"`
	} `json:"subject"`
}

// flattenLangfuseScores rewrites v3 rows into flat objects keyed the way
// collect.mapping expects. Apply() resolves mapping keys with a plain
// map lookup and cannot walk into `subject`, so the trace id is lifted
// to the top level here rather than leaking vendor shape into the
// manifest.
func flattenLangfuseScores(scores []langfuseScore) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(scores))
	for _, s := range scores {
		traceID := s.Subject.TraceID
		if traceID == "" && s.Subject.Kind == "trace" {
			// Trace-level scores carry the id directly; only
			// observation-level rows also populate traceId.
			traceID = s.Subject.ID
		}
		if traceID == "" || s.Name == "" {
			// Without a trace id the score cannot be joined to a Nexus
			// trace, so it would land as an orphan row.
			continue
		}
		flat := map[string]any{
			"name":     s.Name,
			"traceId":  traceID,
			"dataType": s.DataType,
		}
		if s.Comment != "" {
			flat["comment"] = s.Comment
		}
		if score, label, ok := langfuseValue(s.Value); ok {
			flat["value"] = score
			if label != "" {
				flat["label"] = label
			}
		}
		raw, err := json.Marshal(flat)
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// langfuseValue normalises v3's polymorphic value. Numeric scores map
// straight through; booleans become 1/0 with a pass/fail label so the
// sink's Passed flag is meaningful; categorical and text values carry no
// number and are surfaced as a label only.
func langfuseValue(raw json.RawMessage) (float64, string, bool) {
	if len(raw) == 0 {
		return 0, "", false
	}
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		return num, "", true
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			return 1, "pass", true
		}
		return 0, "fail", true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return 0, s, true
	}
	return 0, "", false
}
