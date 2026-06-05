package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestForProviderStripsNexusEval(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: "user", Content: "hi"}},
		NexusEval: &NexusEvalContext{
			Contexts:  []string{"doc1"},
			Reference: "truth",
		},
	}
	b, err := json.Marshal(req.ForProvider())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "nexus_eval") || strings.Contains(string(b), "doc1") {
		t.Fatalf("nexus_eval leaked to provider payload: %s", b)
	}
}

func TestNexusEvalUnmarshal(t *testing.T) {
	raw := `{"model":"m","messages":[{"role":"user","content":"q"}],"nexus_eval":{"contexts":["c1"],"reference":"ref"}}`
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	if req.NexusEval == nil || len(req.NexusEval.Contexts) != 1 || req.NexusEval.Reference != "ref" {
		t.Fatalf("unexpected: %+v", req.NexusEval)
	}
}
