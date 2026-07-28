// Langfuse plugin adapter (Phase F).
//
// Langfuse has two well-documented surfaces:
//   1) Trace ingestion: OTLP-compatible POST /api/public/ingestion
//   2) Score push:       POST /api/public/scores with
//                          {name, value, traceId|observationId|sessionId, dataType, comment}
//
// We expose the same Transmit/Collect pattern as LangSmith and pin
// `dataType: NUMERIC` for the typical numeric score so Langfuse's
// UI pie chart works.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func langfuseTransmit(ctx context.Context, endpoint string, payload map[string]any) error {
	url := joinEndpoint(endpoint, "/api/public/scores")
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "langfuse", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "langfuse", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "langfuse", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &adapterError{
			vendor: "langfuse",
			code:   "status_" + http.StatusText(resp.StatusCode),
			err:    errors.New(resp.Status),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func langfuseFetch(ctx context.Context, endpoint string) ([]json.RawMessage, error) {
	// Polling-mode consumers use Langfuse's `GET /api/public/scores`
	// endpoint — pagination parameters filtered by traceId/createdAt.
	// A v0.7.1 implementation reads the latest batch; for now the
	// collector's heartbeat path is enough to keep the registration
	// alive.
	url := joinEndpoint(endpoint, "/api/public/scores?limit=1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &adapterError{vendor: "langfuse", code: "prepare", err: err}
	}
	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return nil, &adapterError{vendor: "langfuse", code: "send", err: err}
	}
	defer resp.Body.Close()
	return nil, nil
}
