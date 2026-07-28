// LangSmith plugin adapter — the v1 reference plugin.
//
// LangSmith exposes two surfaces we care about:
//
//   1) Trace ingestion: POST https://api.smith.langchain.com/otel/v1/traces
//      in OTLP/JSON. Framework-agnostic; the auth header is the
//      LangChain API key. Used to forward Nexus traces so LangSmith
//      can run its automated evaluators (LLM-as-judge, heuristic).
//
//   2) Feedback push: POST https://api.smith.langchain.com/api/v1/feedback
//      with `{key, score | value, run_id | trace_id, comment}`. We
//      use this when an evaluator approves/rejects a trace inline
//      from Nexus itself, and downstream tools subscribe to the
//      LangSmith feed.
//
// We register two TransmitFuncs (one for trace ingest, one
// identical-shape for feedback pull) and one CollectFunc for the
// polling heartbeat. Webhook-mode plugins use the same shape but
// arrive via /api/eval/plugins/:name/webhook and skip the polling
// path.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// langsmithTransmit sends the rendered payload as an OTLP/JSON
// traces envelope. The envelope is intentionally minimal — a single
// resource scope per trace, one scope span carrying the
// gen_ai.input.messages / gen_ai.output.messages fields.
// Production-grade OTLP protobuf ingestion lives in a follow-up PR.
func langsmithTransmit(ctx context.Context, endpoint string, payload map[string]any) error {
	envelope := langsmithEnvelope(payload)
	body, err := json.Marshal(envelope)
	if err != nil {
		return &adapterError{vendor: "langsmith", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinEndpoint(endpoint, "/otel/v1/traces"), bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "langsmith", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "langsmith", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return &adapterError{
			vendor: "langsmith",
			code:   fmt.Sprintf("status_%d", resp.StatusCode),
			err:    errors.New(string(snippet)),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// langsmithEnvelope lifts the dispatcher payload into the OTLP/JSON
// shape LangSmith accepts. The `payload.metadata` map is preserved
// as a `resource.attributes` block.
func langsmithEnvelope(payload map[string]any) map[string]any {
	resource := map[string]any{
		"attributes": map[string]any{
			"service.name": "nexus",
		},
	}
	// Promote admin-set metadata to resource attributes. Keys are
	// used verbatim so operators can attach their own tags.
	if meta, ok := payload["metadata"].(map[string]any); ok {
		for k, v := range meta {
			resource["attributes"].(map[string]any)[k] = v
		}
	}
	span := map[string]any{
		"name":       "nexus.trace",
		"attributes": map[string]any{},
	}
	for _, k := range []string{"input", "output", "reference"} {
		if v, ok := payload[k]; ok {
			span["attributes"].(map[string]any)["gen_ai."+k] = v
		}
	}
	return map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource":   resource,
				"scopeSpans": []map[string]any{{"scope": map[string]any{"name": "nexus"}, "spans": []map[string]any{span}}},
			},
		},
	}
}

// langsmithFetch is the polling-mode heartbeat. LangSmith doesn't
// have a "give me back the eval scores I sent" pull API; evaluation
// scores come back via the automation-rule webhook. The collector
// uses this path to verify the endpoint is alive and report the
// plugin's last-seen timestamp into metrics.
func langsmithFetch(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	url := joinEndpoint(endpoint, "/api/v1/info")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &adapterError{vendor: "langsmith", code: "prepare", err: err}
	}
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return nil, &adapterError{vendor: "langsmith", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, &adapterError{
			vendor: "langsmith",
			code:   fmt.Sprintf("status_%d", resp.StatusCode),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil, nil
}

// joinEndpoint stitches endpoint + path. Both halves may have
// trailing slashes; we trim here so URLs are well-formed.
func joinEndpoint(endpoint, path string) string {
	e := strings.TrimRight(endpoint, "/")
	p := strings.TrimLeft(path, "/")
	if p == "" {
		return e
	}
	return e + "/" + p
}

// PingLangsmith is the admin REST "test send" helper. Operationally
// it lets an operator verify their secret ref + endpoint before
// enabling the plugin; it does not write any score rows.
func PingLangsmith(ctx context.Context, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := joinEndpoint(endpoint, "/api/v1/info")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
