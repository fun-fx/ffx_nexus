package evalplugin

import (
	"testing"
)

func TestHasFlag(t *testing.T) {
	if !hasFlag([]string{"strict"}, "strict") {
		t.Fatal("expected match for \"strict\" flag")
	}
	if !hasFlag([]string{" strict "}, "strict") {
		t.Fatal("whitespace should not break flag match")
	}
	if hasFlag([]string{"STRICT"}, "strict") {
		t.Fatal("flag matching is case-sensitive; STRICT != strict")
	}
	if hasFlag(nil, "strict") {
		t.Fatal("nil flag slice must not match")
	}
}

func TestDecodeStrictUnknownFieldCapture(t *testing.T) {
	manifest := []byte(`apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: typo-test
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      keyRef: lf-pk|sk
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
  flags: [strict]
  endemic: rare-spelling-typo
  firingRate: 1h
`)
	p, err := DecodeStrict(manifest)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(p.Spec.UnknownFields) != 2 {
		t.Fatalf("expected 2 unknown fields, got %d (%v)", len(p.Spec.UnknownFields), p.Spec.UnknownFields)
	}
	want := map[string]bool{"endemic": true, "firingRate": true}
	for _, f := range p.Spec.UnknownFields {
		if !want[f] {
			t.Errorf("unexpected unknown field %q", f)
		}
	}
}

func TestDecodeStrictWithoutStrictFlagNoCapture(t *testing.T) {
	manifest := []byte(`apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: typo-test
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      keyRef: lf-pk|sk
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
  endemic: rare-spelling-typo
`)
	p, err := DecodeStrict(manifest)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	// Without the strict flag we still capture, but Validate
	// does not emit warnings because `hasFlag(strict)` is false.
	if len(p.Spec.UnknownFields) != 1 {
		t.Fatalf("expected 1 unknown field captured, got %d", len(p.Spec.UnknownFields))
	}
}

func TestStrictSinkCalls(t *testing.T) {
	manifest := []byte(`apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: typo-test
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      keyRef: lf-pk|sk
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
  flags: [strict]
  endemic: rare
`)
	var captured []string
	old := StrictFieldSink
	StrictFieldSink = func(_ string, field string) {
		captured = append(captured, field)
	}
	defer func() { StrictFieldSink = old }()

	if _, err := DecodeStrict(manifest); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("expected StrictFieldSink to fire at least once")
	}
	found := false
	for _, f := range captured {
		if f == "endemic" {
			found = true
		}
	}
	if !found {
		t.Errorf("StrictFieldSink missed 'endemic'; captured=%v", captured)
	}
}

func TestDecodeManyStrict(t *testing.T) {
	raw := []byte(`apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: alpha
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth: { keyRef: lf-pk|sk }
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
  flags: [strict]
  endemic: rare
---
apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: beta
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth: { keyRef: lf-pk|sk }
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
`)
	plugins, err := DecodeManyStrict(raw)
	if err != nil {
		t.Fatalf("DecodeManyStrict: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}
	if len(plugins[0].Spec.UnknownFields) != 1 || plugins[0].Spec.UnknownFields[0] != "endemic" {
		t.Errorf("plugin[0] unknown fields=%v want [endemic]", plugins[0].Spec.UnknownFields)
	}
	if len(plugins[1].Spec.UnknownFields) != 0 {
		t.Errorf("plugin[1] should have no unknowns, got %v", plugins[1].Spec.UnknownFields)
	}
}
