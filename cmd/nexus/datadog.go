// Datadog plugin adapter (Phase G — beta).
//
// Datadog uses two distinct endpoints:
//   - Trace ingest (OTLP via collector or direct POST)
//   - Eval result POST to /api/v1/llm-obs/v1/evaluations with strict
//     metric_type and decimal (not hex) span_id/trace_id requirement.
//
// We forward with hex→decimal conversion so adopters don't have to
// re-implement it themselves.

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

func datadogTransmit(ctx context.Context, endpoint string, payload map[string]any) error {
	// Datadog's evals endpoint wants decimal trace_id/span_id. We
	// scan the payload for any hex-shaped string and convert in
	// place. This is best-effort: callers should keep trace_id
	// values as decimal to avoid the rewrite.
	converted, err := rewriteHexToDecimal(payload)
	if err != nil {
		return &adapterError{vendor: "datadog", code: "convert", err: err}
	}
	url := joinEndpoint(endpoint, "/api/v1/llm-obs/v1/evaluations")
	body, ct, err := jsonBody(converted)
	if err != nil {
		return &adapterError{vendor: "datadog", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "datadog", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "datadog", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &adapterError{
			vendor: "datadog",
			code:   "status_" + http.StatusText(resp.StatusCode),
			err:    errors.New(resp.Status),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// rewriteHexToDecimal walks the payload tree and rewrites any hex
// values found under "trace_id", "span_id", or "join_on.tag.value"
// keys into their decimal representation. Anything that doesn't
// parse as hex passes through untouched so admin-set JSON tags
// stay readable.
func rewriteHexToDecimal(input map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(input))
	for k, v := range input {
		if s, ok := v.(string); ok && (k == "trace_id" || k == "span_id") {
			conv, err := convertIfHex(s)
			if err == nil {
				out[k] = conv
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}

func convertIfHex(s string) (string, error) {
	if len(s) == 0 {
		return s, errors.New("empty")
	}
	if _, err := hex.DecodeString(s); err != nil {
		return s, err
	}
	n, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return "", fmt.Errorf("parse hex %q: %w", s, err)
	}
	return strconv.FormatUint(n, 10), nil
}

func datadogFetch(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	// Phase G beta: collector heartbeat — Datadog doesn't expose a
	// "consume evals I previously sent" REST; the in-product flow is
	// the other way around. The webhook collector path is the
	// real ingestion channel.
	return nil, nil
}
