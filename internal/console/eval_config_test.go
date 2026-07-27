package console

import (
	"encoding/json"
	"testing"
)

// Exercises the dual form of /api/eval/config PATCH: flat top-level
// pii_enabled / completeness_enabled vs nested { "eval": { ... } }.
// Both shapes need to round-trip into identical EvalConfigPatch values
// because the runtime controller merges them at Apply() time.
func TestEvalConfigPatch_DualForm(t *testing.T) {
	flatJSON := []byte(`{"pii_enabled":false,"completeness_enabled":true}`)
	var flat EvalConfigPatch
	if err := json.Unmarshal(flatJSON, &flat); err != nil {
		t.Fatalf("flat unmarshal: %v", err)
	}
	if flat.PIIEnabled == nil || *flat.PIIEnabled != false {
		t.Fatalf("flat PIIEnabled = %v, want &false", flat.PIIEnabled)
	}
	if flat.CompletenessEnabled == nil || *flat.CompletenessEnabled != true {
		t.Fatalf("flat CompletenessEnabled = %v, want &true", flat.CompletenessEnabled)
	}
	if flat.Eval != nil {
		t.Fatalf("flat payload leaked into Eval nested field: %+v", flat.Eval)
	}

	nestedJSON := []byte(`{"eval":{"pii_enabled":false,"completeness_enabled":true}}`)
	var nested EvalConfigPatch
	if err := json.Unmarshal(nestedJSON, &nested); err != nil {
		t.Fatalf("nested unmarshal: %v", err)
	}
	if nested.Eval == nil {
		t.Fatalf("nested payload did not populate Eval: %+v", nested)
	}
	if nested.Eval.PIIEnabled == nil || *nested.Eval.PIIEnabled != false {
		t.Fatalf("nested Eval.PIIEnabled = %v, want &false", nested.Eval.PIIEnabled)
	}
	if nested.Eval.CompletenessEnabled == nil || *nested.Eval.CompletenessEnabled != true {
		t.Fatalf("nested Eval.CompletenessEnabled = %v, want &true", nested.Eval.CompletenessEnabled)
	}
	if nested.PIIEnabled != nil || nested.CompletenessEnabled != nil {
		t.Fatalf("nested payload did not stay nested: top-level leaked %+v", nested)
	}
}
