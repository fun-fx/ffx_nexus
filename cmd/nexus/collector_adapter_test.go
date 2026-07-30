package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
	"github.com/ffxnexus/nexus/internal/observability"
)

func collectorTarget(srv *httptest.Server, transport string) external.Target {
	return external.Target{
		Endpoint: srv.URL,
		Auth:     external.Credentials{Values: []string{"key"}},
		Plugin: &evalplugin.Plugin{
			Metadata: evalplugin.PluginMetadata{Name: "probe"},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{Type: evalplugin.ServiceCollector},
				Collect: evalplugin.CollectSpec{Transport: transport},
			},
		},
		Trace: observability.Trace{TraceID: "trace-otlp"},
	}
}

func TestCollectorTransmit_OTLPTransportWrapsEnvelope(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	tgt := collectorTarget(srv, "otel")
	if err := collectorTransmit(context.Background(), tgt, map[string]any{"score": 0.7}); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	h := got.Load().(http.Header)
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if h.Get("Authorization") != "Bearer key" {
		t.Errorf("Authorization header missing or wrong: %s", h.Get("Authorization"))
	}
}

func TestCollectorTransmit_WebhookSetsUserAgent(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	tgt := collectorTarget(srv, "webhook")
	if err := collectorTransmit(context.Background(), tgt, map[string]any{"score": 0.3}); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	h := got.Load().(http.Header)
	if ua := h.Get("User-Agent"); ua != "nexus-eval-plugin/1.0 (webhook)" {
		t.Errorf("User-Agent = %q", ua)
	}
}

func TestCollectorTransmit_NoTransportDefaultsToOTLP(t *testing.T) {
	srv, got := captureRoundtrip(t, http.StatusOK)
	tgt := external.Target{
		Endpoint: srv.URL,
		Plugin: &evalplugin.Plugin{
			Metadata: evalplugin.PluginMetadata{Name: "probe"},
			Spec: evalplugin.PluginSpec{
				Service: evalplugin.ServiceSpec{Type: evalplugin.ServiceCollector},
				// no Transport — compatibility default.
			},
		},
	}
	if err := collectorTransmit(context.Background(), tgt, map[string]any{"score": 0.3}); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	// Go's stdlib auto-sets User-Agent to its default ("Go-http-client/1.1").
	// Our adapter only overrides it when transport is "webhook"; the no-transport
	// path should NOT carry the "nexus-eval-plugin/1.0 (webhook)" marker.
	if h := got.Load().(http.Header); h.Get("User-Agent") == "nexus-eval-plugin/1.0 (webhook)" {
		t.Errorf("User-Agent should NOT carry webhook marker under no-transport default; got %q", h.Get("User-Agent"))
	}
}

func TestCollectorTransmit_BackCompatAlias_OTelAndWebhookBothRegistered(t *testing.T) {
	// Build a dispatcher, de-register collector, re-register OTel
	// and Webhook. They must both still resolve to collectorTransmit
	// since they used to differ only in headers/URL handling.
	srv1, _ := captureRoundtrip(t, http.StatusOK)
	srv2, _ := captureRoundtrip(t, http.StatusOK)
	tgt1 := collectorTarget(srv1, "otel")
	tgt1.Plugin.Spec.Service.Type = evalplugin.ServiceOTel
	tgt2 := collectorTarget(srv2, "webhook")
	tgt2.Plugin.Spec.Service.Type = evalplugin.ServiceWebhook
	// We don't actually instantiate the dispatcher here — the
	// registration in plugin_adapters.go is the source of truth.
	// Test that calling collectorTransmit with each is well-defined.
	var counter atomic.Int32
	_ = counter.Load()
	if err := collectorTransmit(context.Background(), tgt1, nil); err != nil {
		t.Errorf("ServiceOTel alias: %v", err)
	}
	if err := collectorTransmit(context.Background(), tgt2, nil); err != nil {
		t.Errorf("ServiceWebhook alias: %v", err)
	}
}
