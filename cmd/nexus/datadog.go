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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// PingDatadog verifies the resolved DD-API-KEY is accepted by
// `/api/v1/validate`. Datadog returns 200 with `{"valid":true}` for
// a good key and 403 for a bad one without making a real LLM Obs
// call. The probe also doubles as a region check: if the operator
// pointed us at a non-Datadog host (e.g. a leftover `us3` slug from
// a webhook URL they meant as a different vendor), the call fails at
// the transport layer rather than silently passing.
//
// Earlier releases used genericProbe here, which returned *endpoint
// reachable* against a 403-only host — exactly the shape PR #195
// closed for LangSmith and that this PR closes for the rest of the
// live vendor list.
func PingDatadog(ctx context.Context, endpoint, key string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint not configured")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("no Datadog credential resolved: paste a " +
			"DD-API-KEY")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	url := joinEndpoint(endpoint, "/api/v1/validate")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DD-API-KEY", key)
	resp, err := httpClientForPluginsTest().Do(req)
	if err != nil {
		return fmt.Errorf("probe %s failed at transport layer: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("DD-API-KEY rejected (%s): rotate the key in "+
			"Datadog → Org Settings → API Keys; %s",
			resp.Status, strings.TrimSpace(string(snippet)))
	case resp.StatusCode/100 != 2:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("unexpected status %s from %s: %s",
			resp.Status, url, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func datadogTransmit(ctx context.Context, tgt external.Target, payload map[string]any) error {
	// Datadog's evals endpoint wants decimal trace_id/span_id. We
	// scan the payload for any hex-shaped string and convert in
	// place. This is best-effort: callers should keep trace_id
	// values as decimal to avoid the rewrite.
	converted, err := rewriteHexToDecimal(payload)
	if err != nil {
		return &adapterError{vendor: "datadog", code: "convert", err: err}
	}
	url := joinEndpoint(tgt.Endpoint, "/api/v1/llm-obs/v1/evaluations")
	body, ct, err := jsonBody(converted)
	if err != nil {
		return &adapterError{vendor: "datadog", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "datadog", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	if key := tgt.Auth.Primary(); key != "" {
		// Datadog LLMObs accepts `DD-API-KEY` on /api/v1/llm-obs/*.
		// Bearer is their newer mechanism for /api/unstable/llm-obs
		// paths and we don't hit those yet.
		req.Header.Set("DD-API-KEY", key)
	}
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
