package main

import (
	"net/http"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// httpClientForPlugins returns a shared HTTP client for plugin
// adapters used for real production traffic (dispatcher uploads
// of trace packets, collector polls/webhooks). The 30s ceiling
// matches the typical SLA of vendor APIs so a slow vendor
// doesn't truncate a payload mid-flight.
func httpClientForPlugins() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// httpClientForPluginsTest returns the client used by the admin
// "Test" button's probe. The probe returns a single HTTP status
// that an operator's eyes look at; it doesn't move a payload. It
// must terminate *fast* because the request is synchronous and
// is served back through the Cloudflare → tunnel → /api/eval/...
// chain. Cloudflare's tunnel response deadline in our deployment
// is ~60s (observed via `Retry-After: 60` on a stalled probe that
// hit the deadline). If our probe ran for 30s, there would be
// only 30s left for the JSON encoder, the tunnel retransmit, and
// the browser render. Capping the probe at 8s keeps the entire
// request inside the tunnel's deadline while still leaving a
// generous window for legitimate slow-but-successful responses.
func httpClientForPluginsTest() *http.Client {
	return &http.Client{Timeout: 8 * time.Second}
}

// registerPluginAdapters wires the supported service-type adapters
// into the dispatcher (transmit) and collector (fetch). Each adapter
// is a small encoding-only layer — no third-party SDK ships in the
// gateway binary.
//
// Vendors without a pull API (Datadog, Braintrust, Arize, Confident
// AI, Arize Phoenix) are NOT registered for collect. The Collector
// treats a missing CollectFunc as "no reverse channel — scores
// come in only via the vendor's webhook push, if configured". This
// is safer than registering a `return nil, nil` no-op, which would
// invite future authors to copy-paste the shape and forget to wire
// the actual fetch endpoint.
func registerPluginAdapters(d *external.Dispatcher, c *external.Collector) {
	// LangSmith — webhook reverse channel + hearbeat pull probe.
	d.Register(evalplugin.ServiceLangSmith, langsmithTransmit)
	c.Register(evalplugin.ServiceLangSmith, langsmithFetch)

	// Langfuse — webhook + /api/public/v3/scores polling.
	d.Register(evalplugin.ServiceLangfuse, langfuseTransmit)
	c.Register(evalplugin.ServiceLangfuse, langfuseFetch)

	// Datadog — POST-only; webhook ingest is the only reverse channel.
	d.Register(evalplugin.ServiceDatadog, datadogTransmit)

	// ServiceCollector collapses the old `ServiceOTel` +
	// `ServiceWebhook` split: a single adapter driven by
	// spec.collect.transport ("otel" | "webhook" | "raw").
	//
	// We keep ServiceOTel + ServiceWebhook registered as
	// back-compat aliases — old v1alpha1 manifests shipped with
	// either spelling, and we don't want a "service.type otel"
	// install to watch its collector silently disappear after the
	// operator upgrades to a chart that only knows about
	// otel_collector.
	d.Register(evalplugin.ServiceCollector, collectorTransmit)
	c.Register(evalplugin.ServiceCollector, collectorFetch)
	d.Register(evalplugin.ServiceOTel, collectorTransmit)
	c.Register(evalplugin.ServiceOTel, collectorFetch)
	d.Register(evalplugin.ServiceWebhook, collectorTransmit)
	c.Register(evalplugin.ServiceWebhook, collectorFetch)

	// Braintrust + Arize Phase G — POST-only; webhook collect only.
	d.Register(evalplugin.ServiceBraintrust, braintrustTransmit)
	d.Register(evalplugin.ServiceArize, arizeTransmit)

	// Confident AI + Arize Phoenix (v1alpha2) — both speak OTLP with
	// Basic/Bearer auth variants. Confident AI is DeepEval-native;
	// Arize Phoenix is the open-source alternative. Both are
	// POST-only; webhook collect is the reverse channel.
	d.Register(evalplugin.ServiceConfidentAI, confidentAITransmit)
	d.Register(evalplugin.ServiceArizePhoenix, arizePhoenixTransmit)
}
