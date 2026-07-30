// Arize Phoenix adapter.
//
// Phoenix exposes an OTLP/HTTP endpoint at /v1/traces (their default
// collector endpoint). Phoenix isn't credential-strict: most self-hosted
// deployments leave the API unauthenticated, hosted Phoenix requires
// either an API key in `Authorization: Bearer …` or HTTP Basic with
// the (space_id, api_key) pair from their cloud console.
//
// We prefer Basic when both halves are present (hosted Phoenix), fall
// back to Bearer (mounted-from-AI-Foundry), and only POST
// unauthenticated if the operator really meant that.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

const arizePhoenixOTLPPath = "/v1/traces"

func arizePhoenixTransmit(ctx context.Context, tgt external.Target, payload map[string]any) error {
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "arize_phoenix", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		joinEndpoint(tgt.Endpoint, arizePhoenixOTLPPath), bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "arize_phoenix", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	// Phoenix adds server headers that LibreLLM-style collectors may
	// rely on; mark the SDK so the dashboard counts Nexus-originated
	// traces separately from manual exports.
	req.Header.Set("User-Agent", "nexus-eval-plugin/1.0 (arize_phoenix)")
	var primary string
	switch {
	case hasPair(tgt.Auth):
		user, pass, _ := tgt.Auth.Pair()
		req.SetBasicAuth(user, pass)
	default:
		primary = tgt.Auth.Primary()
	}
	if primary != "" {
		req.Header.Set("Authorization", "Bearer "+primary)
	}

	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "arize_phoenix", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &adapterError{
			vendor: "arize_phoenix",
			code:   fmt.Sprintf("status_%d", resp.StatusCode),
			err:    errors.New(string(snippet)),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func arizePhoenixFetch(_ context.Context, _ external.Target) ([]json.RawMessage, error) {
	return nil, nil
}
