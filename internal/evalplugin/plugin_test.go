package evalplugin

import (
	"strings"
	"testing"
)

const goodPlugin = `
apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langsmith-judge
spec:
  service:
    type: langsmith
    endpoint: https://api.smith.langchain.com
    auth:
      secretRef: langsmith-api-key
  send:
    trigger: on_trace
    sampling: 0.1
    payload:
      input: "{{ .trace.input_messages }}"
      output: "{{ .trace.output_messages }}"
    redact: [pii]
  collect:
    mode: webhook
    mapping:
      name: "$.key"
      score: "$.score"
      label: "$.value"
      explanation: "$.comment"
      trace_id: "$.trace_id"
  timeout: 30s
`

func TestDecodeHappyPath(t *testing.T) {
	p, err := Decode([]byte(goodPlugin))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Metadata.Name != "langsmith-judge" {
		t.Fatalf("name mismatch: %s", p.Metadata.Name)
	}
	if p.Spec.Service.Type != ServiceLangSmith {
		t.Fatalf("service type: %s", p.Spec.Service.Type)
	}
	if p.Spec.Send.Sampling != 0.1 {
		t.Fatalf("sampling: %v", p.Spec.Send.Sampling)
	}
	if p.Spec.Timeout.Seconds() != 30 {
		t.Fatalf("timeout: %s", p.Spec.Timeout)
	}
	if len(p.Spec.Send.Redact) != 1 || p.Spec.Send.Redact[0] != "pii" {
		t.Fatalf("redact: %v", p.Spec.Send.Redact)
	}
}

func TestDecodeManySplitsDocuments(t *testing.T) {
	raw := goodPlugin + "\n---\n" + strings.Replace(goodPlugin, "langsmith-judge", "langfuse-judge", 1)
	plugins, err := DecodeMany([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}
}

func TestValidateRejectsBadAPIVersion(t *testing.T) {
	bad := strings.Replace(goodPlugin, PluginAPIVersion, "nexus.io/v9999", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "apiVersion") {
		t.Fatalf("expected apiVersion error, got: %v", err)
	}
}

func TestValidateRejectsBadServiceType(t *testing.T) {
	bad := strings.Replace(goodPlugin, "type: langsmith", "type: langsmit", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "service.type") {
		t.Fatalf("expected service.type error, got: %v", err)
	}
}

func TestValidateRejectsInlineKey(t *testing.T) {
	bad := strings.Replace(goodPlugin, "secretRef: langsmith-api-key",
		"secretRef: \"\"\n      keyRef: \"\"\n      inlineKey: hunter2", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "inlineKey") {
		t.Fatalf("expected inlineKey error, got: %v", err)
	}
}

func TestValidateRejectsMissingAuth(t *testing.T) {
	bad := strings.Replace(goodPlugin, "secretRef: langsmith-api-key", "secretRef: \"\"", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("expected auth error, got: %v", err)
	}
}

func TestValidateRejectsBadSampling(t *testing.T) {
	bad := strings.Replace(goodPlugin, "sampling: 0.1", "sampling: 1.5", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "sampling") {
		t.Fatalf("expected sampling error, got: %v", err)
	}
}

func TestValidateRequiresIntervalForPoll(t *testing.T) {
	bad := strings.Replace(goodPlugin, "mode: webhook", "mode: poll", 1)
	// No interval → must reject.
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "interval") {
		t.Fatalf("expected interval error, got: %v", err)
	}
}

func TestValidatePollWithInterval(t *testing.T) {
	raw := strings.Replace(goodPlugin, "mode: webhook", "mode: poll\n    interval: 60s", 1)
	p, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if p.Spec.Collect.Interval.Seconds() != 60 {
		t.Fatalf("interval: %s", p.Spec.Collect.Interval)
	}
}

func TestValidateRejectsReservedName(t *testing.T) {
	bad := strings.Replace(goodPlugin, "name: langsmith-judge", "name: nexus", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-name error, got: %v", err)
	}
}

func TestValidateRejectsBadName(t *testing.T) {
	bad := strings.Replace(goodPlugin, "name: langsmith-judge", "name: Bad Name With Spaces", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Fatalf("expected name-format error, got: %v", err)
	}
}

func TestValidateRejectsUnknownRedact(t *testing.T) {
	bad := strings.Replace(goodPlugin, "redact: [pii]", "redact: [secrets]", 1)
	_, err := Decode([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "redact") {
		t.Fatalf("expected redact error, got: %v", err)
	}
}

func TestRegistryMergeHelmWinsOnConflict(t *testing.T) {
	r := NewRegistry()
	helm := Record{
		Plugin: mustDecode(t, goodPlugin),
		Source: Source{Kind: SourceHelm, Ref: "langsmith.yaml"},
	}
	db := Record{
		Plugin: mustDecode(t, strings.Replace(goodPlugin,
			"endpoint: https://api.smith.langchain.com",
			"endpoint: https://example.invalid", 1)),
		Source: Source{Kind: SourceDatabase},
	}
	// Insert Helm first (so it seeds the slot), then a conflicting
	// DB record. The Merge contract says Helm wins and the DB row is
	// returned via the discarded slice.
	r.Merge([]Record{helm})
	discarded := r.Merge([]Record{db})
	if len(discarded) != 1 || discarded[0].Source.Kind != SourceDatabase {
		t.Fatalf("expected db to be discarded, got: %+v", discarded)
	}
	got, ok := r.Lookup("langsmith-judge")
	if !ok {
		t.Fatal("expected lookup hit")
	}
	if !strings.Contains(got.Plugin.Spec.Service.Endpoint, "langchain.com") {
		t.Fatalf("expected helm to win, got endpoint %s", got.Plugin.Spec.Service.Endpoint)
	}
}

func TestRegistryMergeDatabaseIsReplacedByHelm(t *testing.T) {
	r := NewRegistry()
	db := Record{
		Plugin: mustDecode(t, strings.Replace(goodPlugin,
			"endpoint: https://api.smith.langchain.com",
			"endpoint: https://example.invalid", 1)),
		Source: Source{Kind: SourceDatabase},
	}
	helm := Record{
		Plugin: mustDecode(t, goodPlugin),
		Source: Source{Kind: SourceHelm, Ref: "langsmith.yaml"},
	}
	r.Merge([]Record{db})
	discarded := r.Merge([]Record{helm})
	if len(discarded) != 0 {
		t.Fatalf("expected nothing discarded when helm replaces db, got: %+v", discarded)
	}
	got, _ := r.Lookup("langsmith-judge")
	if !strings.Contains(got.Plugin.Spec.Service.Endpoint, "langchain.com") {
		t.Fatalf("expected helm endpoint, got %s", got.Plugin.Spec.Service.Endpoint)
	}
}

func TestRegistrySetEnabled(t *testing.T) {
	r := NewRegistry()
	r.Merge([]Record{{
		Plugin: mustDecode(t, goodPlugin),
		Source: Source{Kind: SourceHelm},
	}})
	if err := r.SetEnabled("langsmith-judge", false); err != nil {
		t.Fatal(err)
	}
	if got := r.Enabled(); len(got) != 0 {
		t.Fatalf("expected empty enabled set, got %d", len(got))
	}
	if err := r.SetEnabled("missing", true); err == nil {
		t.Fatal("expected error for missing plugin")
	}
}

func TestRegistryEnabledFiltersAndSorts(t *testing.T) {
	r := NewRegistry()
	a := mustDecode(t, goodPlugin)
	a.Metadata.Name = "z-plugin"
	b := mustDecode(t, goodPlugin)
	b.Metadata.Name = "a-plugin"
	r.Merge([]Record{
		{Plugin: a, Source: Source{Kind: SourceHelm}, Enabled: false},
		{Plugin: b, Source: Source{Kind: SourceHelm}, Enabled: true},
	})
	enabled := r.Enabled()
	if len(enabled) != 1 || enabled[0].Plugin.Metadata.Name != "a-plugin" {
		t.Fatalf("expected only a-plugin enabled, got %+v", enabled)
	}
}

func mustDecode(t *testing.T, raw string) *Plugin {
	t.Helper()
	p, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}
