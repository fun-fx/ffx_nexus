// Generic OTLP + webhook adapters (Phase G).
//
// OTLP: forward OTLP-shaped exports directly to an OTel collector
//   (or to a vendor that mirrors OTLP syntax — Honeycomb, Tempo, etc.).
//
// Webhook: POST the rendered payload to a generic HTTPS endpoint
//   so operators can route to anything not on the closed enum.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func otelTransmit(ctx context.Context, endpoint string, payload map[string]any) error {
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "otel", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "otel", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "otel", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &adapterError{
			vendor: "otel",
			code:   "status_" + http.StatusText(resp.StatusCode),
			err:    errors.New(resp.Status),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func otelFetch(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	// OTLP does not expose a pull-API for evaluation results; this
	// path is reserved for future expansion.
	return nil, nil
}

func webhookTransmit(ctx context.Context, endpoint string, payload map[string]any) error {
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "webhook", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "webhook", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("User-Agent", "nexus-eval-plugin/1.0")
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "webhook", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &adapterError{
			vendor: "webhook",
			code:   "status_" + http.StatusText(resp.StatusCode),
			err:    errors.New(resp.Status),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func webhookFetch(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	return nil, nil
}
