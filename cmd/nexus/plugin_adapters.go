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

	// Generic OTLP / webhook receivers delegate to Send + receive.
	d.Register(evalplugin.ServiceOTel, otelTransmit)
	c.Register(evalplugin.ServiceOTel, otelFetch)
	d.Register(evalplugin.ServiceWebhook, webhookTransmit)
	c.Register(evalplugin.ServiceWebhook, webhookFetch)

	// Braintrust + Arize Phase G — same shape, different endpoint.
	d.Register(evalplugin.ServiceBraintrust, braintrustTransmit)
	c.Register(evalplugin.ServiceBraintrust, braintrustFetch)
	d.Register(evalplugin.ServiceArize, arizeTransmit)
	c.Register(evalplugin.ServiceArize, arizeFetch)
}
