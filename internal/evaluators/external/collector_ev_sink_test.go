package external_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

type capturesink struct {
	scores []evals.Score
}

func (c *capturesink) WriteScores(_ context.Context, scores []evals.Score) error {
	c.scores = append(c.scores, scores...)
	return nil
}

type capturesEv struct {
	got []evals.Score
}

func (c *capturesEv) EmitShip(_ context.Context, _ string, sc evals.Score) error {
	c.got = append(c.got, sc)
	return nil
}

func newReg(t *testing.T) *evalplugin.Registry {
	t.Helper()
	return evalplugin.NewRegistry()
}

func TestCollector_AppliesScoresAndFansEvalEvents(t *testing.T) {
	capEv := &capturesEv{}
	sink := &capturesink{}
	reg := newReg(t)
	c := external.NewCollector(reg, sink, http.DefaultClient)
	c.SetEvaluationLogSink(capEv)

	p := &evalplugin.Plugin{
		APIVersion: evalplugin.PluginAPIVersion,
		Kind:       evalplugin.PluginKind,
		Metadata:   evalplugin.PluginMetadata{Name: "langfuse-judge"},
		Spec: evalplugin.PluginSpec{
			Service: evalplugin.ServiceSpec{
				Type:     evalplugin.ServiceLangfuse,
				Endpoint: "https://cloud.langfuse.com",
				Auth:     evalplugin.AuthSpec{SecretRef: "lf", KeyRef: "k1|k2"},
			},
			Send: evalplugin.SendSpec{
				Trigger: "on_trace", Sampling: evalplugin.SamplingFraction(1),
			},
			Collect: evalplugin.CollectSpec{
				Mode: "webhook",
				Mapping: evalplugin.ResultMapping{
					Name:        "name",
					Score:       "value",
					Label:       "dataType",
					Explanation: "comment",
					TraceID:     "traceId",
				},
			},
		},
	}
	reg.Merge([]evalplugin.Record{
		{Plugin: p, Source: evalplugin.Source{Kind: evalplugin.SourceHelm}, Enabled: true},
	})
	body := []byte(`[{"name":"answer-relevance","value":0.83,"dataType":"NUMERIC","traceId":"tr-1","comment":"ok"}]`)
	if err := c.Webhook(p.Metadata.Name, strings.NewReader(string(body))); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	if len(sink.scores) != 1 {
		t.Fatalf("expected 1 score, got %d", len(sink.scores))
	}
	if sink.scores[0].TraceID != "tr-1" {
		t.Errorf("TraceID = %q want tr-1", sink.scores[0].TraceID)
	}
	if len(capEv.got) != 1 {
		t.Fatalf("expected evaluation event emit, got %d", len(capEv.got))
	}
	if capEv.got[0].Metric != "answer-relevance" {
		t.Errorf("Metric = %q want answer-relevance", capEv.got[0].Metric)
	}
}

func TestHTTPLogSink_BatchingAndPOST(t *testing.T) {
	var got atomic.Int32
	var saved atomic.Pointer[string]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		s := string(buf[:n])
		saved.Store(&s)
		got.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lg := slogDiscard()
	sink := evals.NewHTTPLogSink(srv.URL+"/v1/logs", srv.Client(), lg)
	defer sink.Close()

	for i := 0; i < 3; i++ {
		if err := sink.EmitShip(context.Background(),
			"trace-z",
			evals.Score{
				TraceID: "trace-z", Metric: "pii", Evaluator: "heuristic",
				Score: float64(i) / 3.0, Passed: true,
			}); err != nil {
			t.Fatalf("EmitShip: %v", err)
		}
	}

	// Force a flush by waiting for the 2s ticker or by adding a
	// large burst — we just sleep a tad longer than the tick.
	time.Sleep(2500 * time.Millisecond)

	if got.Load() == 0 {
		t.Fatal("expected at least one POST within 2s flush window")
	}
	body := *saved.Load()
	if !strings.Contains(body, "gen_ai.evaluation.result") {
		t.Errorf("body missing semconv event name: %s", body)
	}
	if !strings.Contains(body, "service.name") {
		t.Errorf("resource missing service.name attribute: %s", body)
	}
}

func TestHTTPLogSink_EmptyEndpointIsNoop(t *testing.T) {
	lg := slogDiscard()
	sink := evals.NewHTTPLogSink("", nil, lg)
	defer sink.Close()
	for i := 0; i < 50; i++ {
		_ = sink.EmitShip(context.Background(), "t", evals.Score{Metric: "m"})
	}
	if n := sink.LogsFlushed(); n != 0 {
		t.Errorf("no-op endpoint should not flush; got %d", n)
	}
}

func TestNoopLogSink_Drops(t *testing.T) {
	noop := evals.NoopLogSink{}
	if err := noop.EmitShip(context.Background(), "t", evals.Score{Metric: "m"}); err != nil {
		t.Errorf("NoopLogSink emit returned err: %v", err)
	}
}

func TestFormatEndpointForLogs(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"http://collector:4318":           "http://collector:4318/v1/logs",
		"http://collector:4318/":          "http://collector:4318/v1/logs",
		"http://collector:4318/v1/traces": "http://collector:4318/v1/traces/v1/logs",
	}
	for in, want := range cases {
		got := evals.FormatEndpointForLogs(in)
		if got != want {
			t.Errorf("FormatEndpointForLogs(%q)=%q want %q", in, got, want)
		}
	}
}

func slogDiscard() *slog.Logger {
	return slog.New(discardHandler{})
}

// log discarder — keeps test output clean without depending on
// the slog internals across Go versions.
type discardHandler struct{}

func (discardHandler) Enabled(_ context.Context, _ slog.Level) bool  { return false }
func (discardHandler) Handle(_ context.Context, _ slog.Record) error { return nil }
func (h discardHandler) WithAttrs(_ []slog.Attr) slog.Handler        { return h }
func (h discardHandler) WithGroup(_ string) slog.Handler             { return h }
