// Generic forwarder plugin adapters — historical split across two
// service types (ServiceOTel, ServiceWebhook) collapsed into the
// v1alpha2 ServiceCollector with a Transport-shaped flag.
//
// The differences between OTLP and webhook were tiny:
//   - OTLP: simple POST, no extra headers, expects OTLP/JSON shape
//   - Webhook: simple POST, marks itself with User-Agent, free shape
//
// So instead of two distinct service types we use one
// ServiceCollector and an opaque `kind` (String) flag in
// spec.collect.transport ("otel", "webhook", "raw"). The new
// kind "otel" advertises OTLP acceptance by sending our full
// resourceSpans envelope; "webhook" sends the rendered payload as
// is (Promptfoo-style generic receivers); "raw" sends the same
// JSON but says nothing about the contents — used by adapters that
// don't care about shape (Datadog's older /api/v1/series etc).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// collectorTransmit forwards the rendered payload along with the
// requested transport. The dispatcher's view of the world is that
// the manifest has been validated to require Spec.Service.Type ==
// ServiceCollector and a non-empty Spec.Collect.Transport.
//
// All three transports share the same wire step (POST + expect
// 2xx); the differences are:
//
//   - "otel":   payload is wrapped in an OTLP/JSON envelope.
//   - "webhook": payload is sent as the request body, untouched.
//   - "raw":     payload is sent verbatim (== webhook for now).
//
// The first vendor that needs a fourth transport ("prometheus"…)
// adds a `case` block here. The collect path is identical for all
// transports: vendors don't expose a pull API, the eval score comes
// back via the webhook reverse channel noted by RegisterWebhookRoute.
func collectorTransmit(ctx context.Context, tgt external.Target, payload map[string]any) error {
	body, ct, err := jsonBody(payload)
	if err != nil {
		return vendorErr("collector", "encode", err)
	}

	// OTLP transport wraps the body in the resourceSpans envelope
	// so the collector can demux multiple traces per POST. We
	// always advertise `application/json` so the collector's
	// content-type parser recognises the body as OTLP/JSON.
	//
	// Reusing otlpEnvelope here would require the dispatcher to
	// also pass the trace alongside the payload; instead we emit a
	// minimal envelope that just announces the trace the dispatcher
	// already constructed via OTLPEnvelope when shipping to OTLP-
	// native plugins. The collector's job on the receive side is
	// "be tolerant of fragments"; the JSON here is a fragment.
	if tgt.Plugin != nil && transportOf(tgt.Plugin) == "otel" {
		body, err = wrapOTLPFragment(payload)
		if err != nil {
			return vendorErr("collector", "wrap", err)
		}
		ct = "application/json"
	}

	url := collectorURL(tgt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return vendorErr("collector", "prepare", err)
	}
	req.Header.Set("Content-Type", ct)
	if transportOf(tgt.Plugin) == "webhook" {
		req.Header.Set("User-Agent", "nexus-eval-plugin/1.0 (webhook)")
	}
	if key := tgt.Auth.Primary(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := httpClientForPlugins().Do(req)
	if err != nil {
		return vendorErr("collector", "send", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return &adapterError{
			vendor: "collector",
			code:   fmt.Sprintf("status_%d", resp.StatusCode),
			err:    errors.New(string(snippet)),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func collectorFetch(_ context.Context, _ external.Target) ([]json.RawMessage, error) {
	return nil, nil
}

// transportOf returns the Spec.Collect.Transport string, or an
// empty string when not set. Adapters should treat "" as "otel" so
// old manifests that didn't have a transport still ship the right
// shape.
func transportOf(p *evalplugin.Plugin) string {
	if p == nil {
		return ""
	}
	return string(p.Spec.Collect.Transport)
}

func collectorURL(tgt external.Target) string {
	return joinEndpoint(tgt.Endpoint, "")
}

// wrapOTLPFragment renders the payload as a single-span OTLP
// envelope under the "nexus" scope. The envelope is intentionally
// minimal: a single scopeSpans entry with one span whose attributes
// carry the *raw* payload fields, and trace_id/span_id lifted from
// the trace so the collector can correlate. We do *not* use the
// first-party OTLPEnvelope() here because we want to advertise the
// attributes even when the payload is not a verbatim Trace (the
// dispatcher renders render-templates, not raw traces).
func wrapOTLPFragment(payload map[string]any) ([]byte, error) {
	attrs := []map[string]any{}
	for _, k := range []string{"metric", "score", "evaluator", "trace_id"} {
		if v, ok := payload[k]; ok {
			attrs = append(attrs, map[string]any{
				"key":   "nexus.eval." + k,
				"value": map[string]any{"stringValue": fmt.Sprintf("%v", v)},
			})
		}
	}
	envelope := map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": "nexus"}},
					},
				},
				"scopeSpans": []map[string]any{
					{
						"scope": map[string]any{"name": "nexus.eval"},
						"spans": []map[string]any{
							{
								"name":       "nexus.eval.collector",
								"attributes": attrs,
							},
						},
					},
				},
			},
		},
	}
	return json.Marshal(envelope)
}

func vendorErr(vendor, code string, err error) error {
	return &adapterError{vendor: vendor, code: code, err: err}
}
