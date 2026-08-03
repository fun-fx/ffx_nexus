// Confident AI (DeepEval cloud) adapter.
//
// Confident AI's hosted evaluation service accepts eval results via
// OTLP/HTTP at `/v1/otel/traces`. It accepts both Basic (api_key +
// secret_key) and a single Bearer token, so we try both shapes: the
// spec's auth.keyRef lists a "primary|secondary" pair (Basic) or a
// single value (Bearer). We don't know which until runtime, so we
// prefer the Pair() branch when both are present and fall back to
// Bearer().

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

const confidentAIOTLPPath = "/v1/otel/traces"

// PingConfidentAI verifies the Confident AI (DeepEval cloud) endpoint
// is reachable and the resolved credential is accepted by the
// vendor. We hit `/v1/projects` because it is the cheapest endpoint
// that distinguishes "host alive" from "credentials good", and the
// vendor returns 401 when the API key pair is wrong rather than 200
// with an empty body. The same probe path works for both Bearer and
// Basic auth shapes — Confident AI is happy with either as long as
// `Authorization` carries the right value.
//
// Earlier releases passed plugins through genericProbe, which did not
// attach the credential and ignored the HTTP status. That is exactly
// the false-positive shape PR #195 closed for LangSmith.
func PingConfidentAI(ctx context.Context, endpoint, primary, secondary string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	url := joinEndpoint(endpoint, "/v1/projects")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	switch {
	case primary != "" && secondary != "":
		req.SetBasicAuth(primary, secondary)
	case primary != "":
		req.Header.Set("Authorization", "Bearer "+primary)
	default:
		return errors.New("no Confident AI credential resolved: paste a " +
			"single API key (Bearer) or a public|secret key pair (Basic)")
	}
	resp, err := httpClientForPluginsTest().Do(req)
	if err != nil {
		return fmt.Errorf("probe %s failed at transport layer: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("credentials rejected (%s): check the API key "+
			"(Bearer) or public/secret key pair (Basic); %s",
			resp.Status, strings.TrimSpace(string(snippet)))
	case resp.StatusCode/100 != 2:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("unexpected status %s from %s: %s",
			resp.Status, url, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func confidentAITransmit(ctx context.Context, tgt external.Target, payload map[string]any) error {
	body, ct, err := jsonBody(payload)
	if err != nil {
		return &adapterError{vendor: "confident_ai", code: "encode", err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		joinEndpoint(tgt.Endpoint, confidentAIOTLPPath), bytes.NewReader(body))
	if err != nil {
		return &adapterError{vendor: "confident_ai", code: "prepare", err: err}
	}
	req.Header.Set("Content-Type", ct)
	var primary string
	switch {
	case pairOK(tgt.Auth):
		// DeepEval cloud's service accepts Basic auth with the
		// project's public/secret keys.
		user, pass, _ := tgt.Auth.Pair()
		req.SetBasicAuth(user, pass)
	default:
		primary = tgt.Auth.Primary()
	}
	if primary != "" {
		req.Header.Set("Authorization", "Bearer "+primary)
	}
	if !pairOK(tgt.Auth) && primary == "" {
		return &adapterError{
			vendor: "confident_ai",
			code:   "auth",
			err:    errors.New("needs a single API key or a public_key|secret_key pair"),
		}
	}

	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return &adapterError{vendor: "confident_ai", code: "send", err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &adapterError{
			vendor: "confident_ai",
			code:   fmt.Sprintf("status_%d", resp.StatusCode),
			err:    errors.New(string(snippet)),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
