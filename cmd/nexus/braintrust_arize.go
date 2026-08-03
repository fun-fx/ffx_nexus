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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// PingBraintrust verifies the Braintrust project is reachable AND
// the resolved API key is accepted by `GET /v1/projects`. Braintrust
// returns 401 with an empty body when the bearer is wrong, which is
// why we narrowly check the status code instead of treating any 2xx
// as success.
//
// Earlier releases ran this through genericProbe, which only proved
// the host answered TCP. PR #195 closed the same false-positive for
// LangSmith — these four probes apply the same pattern to the rest
// of the live vendor list so that "Test passes" means "credentials
// verified" everywhere, not "host alive."
func PingBraintrust(ctx context.Context, endpoint, key string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint not configured")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("no Braintrust credential resolved: paste a " +
			"Braintrust API key")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	url := joinEndpoint(endpoint, "/v1/projects")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClientForPluginsTest().Do(req)
	if err != nil {
		return fmt.Errorf("probe %s failed at transport layer: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("credentials rejected (%s): check the Braintrust "+
			"API key; %s", resp.Status, strings.TrimSpace(string(snippet)))
	case resp.StatusCode/100 != 2:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("unexpected status %s from %s: %s",
			resp.Status, url, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func braintrustTransmit(ctx context.Context, tgt external.Target, payload map[string]any) error {
	url := joinEndpoint(tgt.Endpoint, "/v1/project_logs/nexus/feedback")
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "braintrust", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "braintrust", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	if key := tgt.Auth.Primary(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
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

func arizeTransmit(ctx context.Context, tgt external.Target, payload map[string]any) error {
	url := joinEndpoint(tgt.Endpoint, "/v1/span_annotations")
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "arize", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "arize", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	if key := tgt.Auth.Primary(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
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
