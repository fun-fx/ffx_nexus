package observability

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsRecorderProducesValidExpositionFormat(t *testing.T) {
	rec := NewMetricsRecorder(":0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if rec == nil {
		t.Fatal("expected recorder even when addr is :0 (we still want it in memory)")
	}
	t.Cleanup(func() { _ = rec.Close(t.Context()) })

	rec.Record(Trace{
		RequestModel:     "model-a",
		ProviderName:     "openai",
		StatusCode:       200,
		LatencyMs:        120,
		CostUSD:          0.0042,
		CredentialSource: "user",
	})
	rec.Record(Trace{
		RequestModel:     "model-a",
		ProviderName:     "openai",
		StatusCode:       500,
		LatencyMs:        3000,
		ErrorType:        "upstream_error",
		CredentialSource: "user",
	})
	rec.Record(Trace{
		RequestModel: "model-b",
		CacheHit:     true,
		StatusCode:   200,
		LatencyMs:    50,
	})
	rec.RecordFailover("model-a", "model-b", "upstream_error", "pod-A")
	rec.RecordQualityScore("model-a", 0.85)

	srv := httptest.NewServer(http.HandlerFunc(rec.handleMetrics))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)

	for _, want := range []string{
		"nexus_gateway_requests_total",
		"nexus_gateway_request_duration_seconds_bucket",
		"nexus_gateway_cache_hits_total",
		"nexus_gateway_errors_total",
		"nexus_router_failover_total",
		"nexus_gateway_cost_usd_total",
		"nexus_eval_quality_score",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in /metrics output:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `nexus_gateway_request_duration_seconds_bucket{gen_ai_request_model="model-a",le="250"} 1`) {
		t.Errorf("expected one latency observation in the 250ms bucket:\n%s", out)
	}
	if !strings.Contains(out, "# HELP nexus_gateway_requests_total") {
		t.Errorf("expected HELP on requests_total")
	}
	if !strings.Contains(out, "# TYPE nexus_gateway_requests_total counter") {
		t.Errorf("expected counter TYPE on requests_total")
	}
	if !strings.Contains(out, `le="+Inf"`) {
		t.Errorf("expected +Inf le bucket")
	}
}

func TestMetricsRecorderEndpointOptIn(t *testing.T) {
	got := NewMetricsRecorder("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got != nil {
		t.Fatal("empty addr must yield nil recorder (zero-dep fast path preserved)")
	}
}
