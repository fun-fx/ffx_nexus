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

	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

const confidentAIOTLPPath = "/v1/otel/traces"

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
