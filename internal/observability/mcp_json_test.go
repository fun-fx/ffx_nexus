package observability

import (
	"encoding/json"
	"testing"
)

func TestMCPLogPageJSONEmptyItemsAreArray(t *testing.T) {
	page := MCPLogPage{Items: make([]MCPLogSummary, 0)}
	b, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["items"]) != "[]" {
		t.Fatalf("items = %s, want []", raw["items"])
	}
}

func TestMCPLogPageJSONNilItemsWouldBeNull(t *testing.T) {
	// Documents the Go gotcha the reader must not ship: a nil slice marshals
	// as JSON null, which crashed the console table on an empty install.
	b, err := json.Marshal(MCPLogPage{})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["items"]) != "null" {
		t.Fatalf("nil slice should marshal as null; got %s", raw["items"])
	}
}
