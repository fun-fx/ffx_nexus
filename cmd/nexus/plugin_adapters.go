package main

import (
	"net/http"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// httpClientForPlugins returns a shared HTTP client for plugin
// adapters. We keep one per process so the connection pool can be
// reused across dispatcher/collector goroutines.
func httpClientForPlugins() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
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
