package external

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
)

// fakeSink captures whatever Collector.Webhook asks the sink to
// persist. Using a sink here (instead of NoopSink) lets assertions see
// the OTel-shaped records that Apply() actually produced after the
// mapping was applied — that's what admins care about on the UI.
type fakeSink struct {
	scores []evals.Score
}

func (s *fakeSink) WriteScores(_ context.Context, scores []evals.Score) error {
	s.scores = append(s.scores, scores...)
	return nil
}

func TestCollector_Webhook_AppliesMapping(t *testing.T) {
	reg := evalplugin.NewRegistry()
	spec := decode(t, `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
    interval: 60s
    mapping:
      name: name
      score: value
      explanation: comment
      trace_id: traceId
  timeout: 30s
`)
	if discarded := reg.Merge([]evalplugin.Record{{
		Source: evalplugin.Source{Kind: evalplugin.SourceHelm},
		Plugin: spec,
	}}); len(discarded) > 0 {
		t.Fatalf("unexpected discard: %d", len(discarded))
	}
	sink := &fakeSink{}
	c := NewCollector(reg, sink, nil)
	body := bytes.NewBufferString(`{"name":"answer-relevance","value":0.9,"comment":"good","traceId":"abc-1"}`)
	if err := c.Webhook("langfuse-judge", body); err != nil {
		t.Fatalf("Webhook returned err: %v", err)
	}
	if len(sink.scores) != 1 {
		t.Fatalf("expected exactly one score, got %d", len(sink.scores))
	}
	sc := sink.scores[0]
	if sc.Metric != "answer-relevance" {
		t.Errorf("Metric: got %q want %q", sc.Metric, "answer-relevance")
	}
	if sc.Score != 0.9 {
		t.Errorf("Score: got %v want 0.9", sc.Score)
	}
	if sc.Passed != true {
		t.Errorf("Passed: got %v want true", sc.Passed)
	}
	if sc.Rationale != "good" {
		t.Errorf("Rationale: got %q want %q", sc.Rationale, "good")
	}
	if sc.TraceID != "abc-1" {
		t.Errorf("TraceID: got %q want %q", sc.TraceID, "abc-1")
	}
	if !strings.HasPrefix(sc.Evaluator, "plugin:") {
		t.Errorf("Evaluator prefix: got %q want plugin:…", sc.Evaluator)
	}
	if time.Since(sc.Timestamp) > 5*time.Second {
		t.Errorf("Timestamp not recent: %v", sc.Timestamp)
	}
}

func TestCollector_Webhook_BatchArr(t *testing.T) {
	reg := evalplugin.NewRegistry()
	spec := decode(t, `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
    interval: 60s
    mapping:
      name: name
      score: value
      trace_id: traceId
  timeout: 30s
`)
	if discarded := reg.Merge([]evalplugin.Record{{
		Source: evalplugin.Source{Kind: evalplugin.SourceHelm},
		Plugin: spec,
	}}); len(discarded) > 0 {
		t.Fatalf("unexpected discard: %d", len(discarded))
	}
	sink := &fakeSink{}
	c := NewCollector(reg, sink, nil)
	body := bytes.NewBufferString(`[{"name":"x","value":0.4,"traceId":"t1"},{"name":"y","value":0.95,"traceId":"t2"}]`)
	if err := c.Webhook("langfuse-judge", body); err != nil {
		t.Fatalf("Webhook returned err: %v", err)
	}
	if len(sink.scores) != 2 {
		t.Fatalf("expected two scores, got %d", len(sink.scores))
	}
	if sink.scores[0].Passed != false || sink.scores[1].Passed != true {
		t.Errorf("pass thresholds wrong: %+v", sink.scores)
	}
}

func TestCollector_Webhook_RejectsNonWebhookMode(t *testing.T) {
	reg := evalplugin.NewRegistry()
	spec := decode(t, `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: poll-only
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: poll
    interval: 60s
  timeout: 30s
`)
	if discarded := reg.Merge([]evalplugin.Record{{
		Source: evalplugin.Source{Kind: evalplugin.SourceHelm},
		Plugin: spec,
	}}); len(discarded) > 0 {
		t.Fatalf("unexpected discard: %d", len(discarded))
	}
	c := NewCollector(reg, &fakeSink{}, nil)
	body := bytes.NewBufferString(`{"name":"x","value":1.0,"traceId":"t1"}`)
	err := c.Webhook("poll-only", body)
	if err == nil {
		t.Fatal("expected mode-pill plug-in to reject webhook traffic, got nil")
	}
	if !strings.Contains(err.Error(), "webhook") {
		t.Errorf("expected webhook-mode error, got: %v", err)
	}
}

func decode(t *testing.T, body string) *evalplugin.Plugin {
	t.Helper()
	p, err := evalplugin.Decode([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}
