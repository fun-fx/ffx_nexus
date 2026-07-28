package external

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/observability"
)
func TestApplyMapsToOTelRecord(t *testing.T) {
	raw := json.RawMessage(`{"key":"quality","score":0.91,"comment":"nice","trace_id":"abc"}`)
	mapping := evalplugin.ResultMapping{
		Name:        "key",
		Score:       "score",
		Explanation: "comment",
		TraceID:     "trace_id",
	}
	score, err := Apply(raw, mapping, "langsmith-judge")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if score.Metric != "quality" {
		t.Fatalf("metric: %s", score.Metric)
	}
	if score.Score != 0.91 {
		t.Fatalf("score: %f", score.Score)
	}
	if !score.Passed {
		t.Fatalf("passed: expected true at 0.91")
	}
	if score.TraceID != "abc" {
		t.Fatalf("trace: %s", score.TraceID)
	}
	if score.Evaluator != "plugin:langsmith-judge" {
		t.Fatalf("evaluator: %s", score.Evaluator)
	}
}

func TestApplyPassFlagFromLabel(t *testing.T) {
	cases := []struct {
		name   string
		label  string
		want   bool
	}{
		{"pass", "pass", true},
		{"true", "true", true},
		{"fail", "fail", false},
		{"false", "false", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := json.RawMessage(`{"value":"` + c.label + `","score":0.5,"trace_id":"abc"}`)
			mapping := evalplugin.ResultMapping{
				Label:   "value",
				TraceID: "trace_id",
				Score:   "score",
			}
			s, _ := Apply(raw, mapping, "test")
			if s.Passed != c.want {
				t.Fatalf("label=%s passed=%v want=%v", c.label, s.Passed, c.want)
			}
		})
	}
}

func TestRenderPayloadSimpleValue(t *testing.T) {
	out, err := renderPayload(map[string]string{
		"static": "hello",
	}, observability.Trace{})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if out["static"] != "hello" {
		t.Fatalf("static: %v", out["static"])
	}
}

func TestRenderPayloadTemplate(t *testing.T) {
	out, err := renderPayload(map[string]string{
		"content": "{{ index .trace \"gen_ai.input.messages\" }}",
	}, observability.Trace{InputMessages: "hi"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if out["content"] != "hi" {
		t.Fatalf("content: %v", out["content"])
	}
}

func TestDispatcherDispatchesViaRegisteredAdapter(t *testing.T) {
	d := NewDispatcher(nil, nil)
	called := false
	d.Register(evalplugin.ServiceLangSmith,
		func(ctx context.Context, endpoint string, payload map[string]any) error {
			called = true
			return nil
		})
	p := &evalplugin.Plugin{
		APIVersion: evalplugin.PluginAPIVersion,
		Kind:       evalplugin.PluginKind,
		Metadata:   evalplugin.PluginMetadata{Name: "langsmith-judge"},
		Spec: evalplugin.PluginSpec{
			Service: evalplugin.ServiceSpec{
				Type:     evalplugin.ServiceLangSmith,
				Endpoint: "https://api.smith.langchain.com",
				Auth:     evalplugin.AuthSpec{SecretRef: "k"},
			},
			Send: evalplugin.SendSpec{Trigger: "on_trace", Sampling: 1, Payload: map[string]string{"k": "v"}},
			Collect: evalplugin.CollectSpec{Mode: "webhook"},
		},
	}
	if err := d.Dispatch(context.Background(), observability.Trace{}, p); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !called {
		t.Fatal("expected registered adapter to be called")
	}
}

func TestDispatcherRejectsUnknownServiceType(t *testing.T) {
	d := NewDispatcher(nil, nil)
	p := &evalplugin.Plugin{
		APIVersion: evalplugin.PluginAPIVersion,
		Kind:       evalplugin.PluginKind,
		Metadata:   evalplugin.PluginMetadata{Name: "x"},
		Spec: evalplugin.PluginSpec{
			Service: evalplugin.ServiceSpec{Type: "unsupported-type", Endpoint: "https://x"},
			Send:    evalplugin.SendSpec{Sampling: 1},
			Collect: evalplugin.CollectSpec{Mode: "sync"},
		},
	}
	if err := d.Dispatch(context.Background(), observability.Trace{}, p); err == nil {
		t.Fatal("expected error for unregistered service type")
	}
}

func TestReadBodyLimitsSize(t *testing.T) {
	data := make([]byte, 2000)
	for i := range data {
		data[i] = 'a'
	}
	out, err := ReadBody(bytesReader(data), 100)
	if err == nil {
		t.Fatal("expected body too large error")
	}
	if len(out) == 0 {
		t.Fatal("expected partial output")
	}
}

type bytesRdr struct {
	buf []byte
	pos int
}

func bytesReader(b []byte) *bytesRdr { return &bytesRdr{buf: b} }

func (r *bytesRdr) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, ioEOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

var ioEOF = errEOF{}

type errEOF struct{}

func (errEOF) Error() string { return "EOF" }
