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
func registerPluginAdapters(d *external.Dispatcher, c *external.Collector) {
	// LangSmith (Phase D — reference plugin).
	d.Register(evalplugin.ServiceLangSmith, langsmithTransmit)
	c.Register(evalplugin.ServiceLangSmith, langsmithFetch)

	// Langfuse (Phase F — second reference plugin).
	d.Register(evalplugin.ServiceLangfuse, langfuseTransmit)
	c.Register(evalplugin.ServiceLangfuse, langfuseFetch)

	// Datadog — Phase G beta.
	d.Register(evalplugin.ServiceDatadog, datadogTransmit)
	c.Register(evalplugin.ServiceDatadog, datadogFetch)

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

	// Braintrust + Arize Phase G — same shape, different endpoint.
	d.Register(evalplugin.ServiceBraintrust, braintrustTransmit)
	c.Register(evalplugin.ServiceBraintrust, braintrustFetch)
	d.Register(evalplugin.ServiceArize, arizeTransmit)
	c.Register(evalplugin.ServiceArize, arizeFetch)

	// Confident AI + Arize Phoenix (v1alpha2) — both speak OTLP with
	// Basic/Bearer auth variants. Confident AI is DeepEval-native;
	// Arize Phoenix is the open-source alternative.
	d.Register(evalplugin.ServiceConfidentAI, confidentAITransmit)
	c.Register(evalplugin.ServiceConfidentAI, confidentAIFetch)
	d.Register(evalplugin.ServiceArizePhoenix, arizePhoenixTransmit)
	c.Register(evalplugin.ServiceArizePhoenix, arizePhoenixFetch)

	// ServiceCollector envelopes the same wire-level transmit logic
	// as ServiceOTel; the difference is the v1alpha2 manifest carries
	// spec.collect.transport, which downstream collectors use to pick
	// spans from the envelope by attribute.
	d.Register(evalplugin.ServiceCollector, collectorTransmit)
	c.Register(evalplugin.ServiceCollector, collectorFetch)
}
