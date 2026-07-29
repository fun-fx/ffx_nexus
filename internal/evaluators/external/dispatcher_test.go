package external

import (
	"context"
	"encoding/json"
	"errors"
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
		name  string
		label string
		want  bool
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
	d.SetSecretResolver(stubResolver{creds: Credentials{Values: []string{"pk", "sk"}}})
	called := false
	var gotTarget Target
	d.Register(evalplugin.ServiceLangSmith,
		func(ctx context.Context, tgt Target, payload map[string]any) error {
			called = true
			gotTarget = tgt
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
			Send:    evalplugin.SendSpec{Trigger: "on_trace", Sampling: 1, Payload: map[string]string{"k": "v"}},
			Collect: evalplugin.CollectSpec{Mode: "webhook"},
		},
	}
	if err := d.Dispatch(context.Background(), observability.Trace{}, p); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !called {
		t.Fatal("expected registered adapter to be called")
	}
	if got, _, ok := gotTarget.Auth.Pair(); !ok || got != "pk" {
		t.Errorf("adapter received no credentials (%q, ok=%v); an adapter that "+
			"cannot authenticate is why every vendor call used to 401", got, ok)
	}
	if gotTarget.Endpoint != "https://api.smith.langchain.com" {
		t.Errorf("Endpoint = %q", gotTarget.Endpoint)
	}
}

// A manifest that declares auth must not fall back to sending an
// unauthenticated request: the vendor rejects it out of sight, which
// reads as "the plugin works but the vendor has no data".
func TestDispatchFailsWhenAuthDeclaredButNoResolver(t *testing.T) {
	d := NewDispatcher(nil, nil)
	d.Register(evalplugin.ServiceLangfuse,
		func(ctx context.Context, tgt Target, payload map[string]any) error {
			t.Fatal("adapter must not be called without credentials")
			return nil
		})
	p := &evalplugin.Plugin{
		APIVersion: evalplugin.PluginAPIVersion,
		Kind:       evalplugin.PluginKind,
		Metadata:   evalplugin.PluginMetadata{Name: "langfuse-judge"},
		Spec: evalplugin.PluginSpec{
			Service: evalplugin.ServiceSpec{
				Type:     evalplugin.ServiceLangfuse,
				Endpoint: "https://cloud.langfuse.com",
				Auth:     evalplugin.AuthSpec{SecretRef: "langfuse-creds", KeyRef: "public_key|secret_key"},
			},
			Send:    evalplugin.SendSpec{Trigger: "on_trace", Sampling: 1},
			Collect: evalplugin.CollectSpec{Mode: "poll"},
		},
	}
	err := d.Dispatch(context.Background(), observability.Trace{}, p)
	if !errors.Is(err, ErrNoSecretResolver) {
		t.Fatalf("err = %v, want ErrNoSecretResolver", err)
	}
}

// A manifest with no auth block still dispatches: self-hosted
// collectors that need no credential must keep working.
func TestDispatchWithoutAuthNeedsNoResolver(t *testing.T) {
	d := NewDispatcher(nil, nil)
	called := false
	d.Register(evalplugin.ServiceOTel,
		func(ctx context.Context, tgt Target, payload map[string]any) error {
			called = true
			if !tgt.Auth.Empty() {
				t.Error("expected empty credentials")
			}
			return nil
		})
	p := &evalplugin.Plugin{
		APIVersion: evalplugin.PluginAPIVersion,
		Kind:       evalplugin.PluginKind,
		Metadata:   evalplugin.PluginMetadata{Name: "otel-local"},
		Spec: evalplugin.PluginSpec{
			Service: evalplugin.ServiceSpec{
				Type:     evalplugin.ServiceOTel,
				Endpoint: "http://otel-collector:4318/v1/traces",
			},
			Send:    evalplugin.SendSpec{Trigger: "on_trace", Sampling: 1},
			Collect: evalplugin.CollectSpec{Mode: "webhook"},
		},
	}
	if err := d.Dispatch(context.Background(), observability.Trace{}, p); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !called {
		t.Fatal("expected adapter to be called")
	}
}

type stubResolver struct {
	creds Credentials
	err   error
}

func (s stubResolver) Resolve(context.Context, evalplugin.AuthSpec) (Credentials, error) {
	return s.creds, s.err
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
