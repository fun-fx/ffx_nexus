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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

const arizePhoenixOTLPPath = "/v1/traces"

// PingArizePhoenix touches the vendor's `/v1/traces` endpoint with an
// HTTP GET rather than the OTLP POST we use for traffic. Phoenix always
// returns 405 Method Not Allowed for GET on the OTLP collector; that's
// the cheapest signal the route exists. With a credential we attempt
// Basic first (hosted) and fall back to Bearer.
//
// The "Test" button previously reached this vendor through
// genericProbe, which did not distinguish host alive from auth
// accepted. With this fix the operator sees *credentials rejected*
// when the key is wrong rather than *endpoint reachable*.
func PingArizePhoenix(ctx context.Context, endpoint, primary, secondary string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	url := joinEndpoint(endpoint, "/v1/traces")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "nexus-eval-plugin-tester/1.0 (+https://nexus)")
	switch {
	case primary != "" && secondary != "":
		req.SetBasicAuth(primary, secondary)
	case primary != "":
		req.Header.Set("Authorization", "Bearer "+primary)
	case primary == "" && secondary == "":
		// Self-hosted Phoenix frequently has no auth — accept that
		// the operator meant it.
	default:
		return errors.New("incomplete Arize Phoenix credential: paste a " +
			"single API key (Bearer) or a space_id|api_key pair (Basic), " +
			"or leave both blank for an unauthenticated self-host")
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
			"(Bearer) or space_id|api_key pair (Basic); %s",
			resp.Status, strings.TrimSpace(string(snippet)))
	// Phoenix returns 405 Method Not Allowed when an unauthenticated
	// GET hits /v1/traces — that's the route also accepting POST OTLP,
	// which is exactly what we want to confirm.
	case resp.StatusCode == http.StatusMethodNotAllowed:
		return nil
	case resp.StatusCode/100 != 2:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("unexpected status %s from %s: %s",
			resp.Status, url, strings.TrimSpace(string(snippet)))
	}
	return nil
}

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
	case pairOK(tgt.Auth):
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

// pairOK is a friendly wrapper around Credentials.Pair() that
// returns true only when both halves resolved to non-empty
// strings. Shared between confident_ai.go and arize_phoenix.go —
// a stub function whose only job is to make the switch arm
// readable ("Pair() returns 'ok'") rather than have to inline
// three return-value destructuring inside the case body.
func pairOK(c external.Credentials) bool { _, _, ok := c.Pair(); return ok }
