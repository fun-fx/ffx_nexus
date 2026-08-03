package evalplugin

import (
	"strings"
	"testing"
)

func heuristicManifest(metric string) string {
	return `apiVersion: nexus.io/v1alpha2
kind: EvalPlugin
metadata:
  name: heuristic-probe
spec:
  service:
    type: heuristic
    metric:
      name: ` + metric + `
      args:
        needle: hello
  send:
    trigger: on_trace
    sampling: 1.0
  collect:
    mode: webhook
`
}

// TestRetiredPythonMetricsAreRejectedWithGuidance covers the manifests
// already written against the old enum. A bare "not supported" would
// leave an operator guessing whether they typo'd; the error has to say
// the metric is gone and what to use instead.
func TestRetiredPythonMetricsAreRejectedWithGuidance(t *testing.T) {
	for _, metric := range []string{"hf_evaluate", "lighteval", "ragas"} {
		_, err := Decode([]byte(heuristicManifest(metric)))
		if err == nil {
			t.Fatalf("metric %q should be rejected", metric)
		}
		msg := err.Error()
		if !strings.Contains(msg, "was removed") {
			t.Errorf("metric %q: error should say it was removed, got %v", metric, msg)
		}
		if !strings.Contains(msg, "rouge_l") {
			t.Errorf("metric %q: error should list the surviving metrics, got %v", metric, msg)
		}
	}
}

// TestInProcessMetricsStillValidate is the control: the four pure-Go
// metrics are the whole surviving surface.
func TestInProcessMetricsStillValidate(t *testing.T) {
	for _, metric := range []string{"contains", "pii", "exact_match", "rouge_l"} {
		if _, err := Decode([]byte(heuristicManifest(metric))); err != nil {
			t.Errorf("metric %q should validate: %v", metric, err)
		}
	}
}

// TestUnknownMetricStillRejected keeps the retired-name branch from
// swallowing plain typos.
func TestUnknownMetricStillRejected(t *testing.T) {
	_, err := Decode([]byte(heuristicManifest("rouge_k")))
	if err == nil {
		t.Fatal("a typo should be rejected")
	}
	if strings.Contains(err.Error(), "was removed") {
		t.Errorf("a typo is not a retired metric: %v", err)
	}
}
