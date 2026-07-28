// Braintrust + Arize plugin adapters (Phase G).
//
// Braintrust surface:
//   1) /v1/project_logs/<id>/feedback (feedback POST)
//   2) /otel/v1/traces (OTLP)
//
// Arize Phoenix/AX surface:
//   1) /v1/span_annotations (annotations POST)
//   2) /v1/trace_annotations

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func braintrustTransmit(ctx context.Context, endpoint string, payload map[string]any) error {
	url := joinEndpoint(endpoint, "/v1/project_logs/nexus/feedback")
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "braintrust", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "braintrust", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "braintrust", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &adapterError{
			vendor: "braintrust",
			code:   "status_" + http.StatusText(resp.StatusCode),
			err:    errors.New(resp.Status),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func braintrustFetch(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	return nil, nil
}

func arizeTransmit(ctx context.Context, endpoint string, payload map[string]any) error {
	url := joinEndpoint(endpoint, "/v1/span_annotations")
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "arize", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "arize", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "arize", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &adapterError{
			vendor: "arize",
			code:   "status_" + http.StatusText(resp.StatusCode),
			err:    errors.New(resp.Status),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func arizeFetch(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	return nil, nil
}
