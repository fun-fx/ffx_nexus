package guardrails

import "testing"

func newJSONGuard() *Guard {
	return New(Config{Enabled: true, ValidateJSONOutput: true})
}

func TestCheckJSONOutputDisabled(t *testing.T) {
	g := New(Config{Enabled: true}) // ValidateJSONOutput off
	if f := g.CheckJSONOutput("not json", nil); f.Blocked {
		t.Fatal("disabled JSON validation must not block")
	}
}

func TestCheckJSONOutputNilGuard(t *testing.T) {
	var g *Guard
	if f := g.CheckJSONOutput("not json", nil); f.Blocked {
		t.Fatal("nil guard must not block")
	}
}

func TestCheckJSONOutputValidObject(t *testing.T) {
	g := newJSONGuard()
	if f := g.CheckJSONOutput(`{"answer":42}`, nil); f.Blocked {
		t.Fatalf("valid JSON should pass, got %+v", f)
	}
}

func TestCheckJSONOutputInvalid(t *testing.T) {
	g := newJSONGuard()
	f := g.CheckJSONOutput("here is your answer: 42", nil)
	if !f.Blocked || f.Rule != "json_output" {
		t.Fatalf("invalid JSON should block with json_output, got %+v", f)
	}
}

func TestCheckJSONOutputEmpty(t *testing.T) {
	g := newJSONGuard()
	if f := g.CheckJSONOutput("", nil); !f.Blocked {
		t.Fatal("empty output should block when JSON requested")
	}
}

func TestCheckJSONSchemaConforms(t *testing.T) {
	g := newJSONGuard()
	schema := []byte(`{
		"type":"object",
		"properties":{"name":{"type":"string"},"age":{"type":"integer"}},
		"required":["name","age"]
	}`)
	if f := g.CheckJSONOutput(`{"name":"Ada","age":36}`, schema); f.Blocked {
		t.Fatalf("conforming output should pass, got %+v", f)
	}
}

func TestCheckJSONSchemaViolation(t *testing.T) {
	g := newJSONGuard()
	schema := []byte(`{
		"type":"object",
		"properties":{"name":{"type":"string"},"age":{"type":"integer"}},
		"required":["name","age"]
	}`)
	// Missing required "age".
	f := g.CheckJSONOutput(`{"name":"Ada"}`, schema)
	if !f.Blocked || f.Rule != "json_schema" {
		t.Fatalf("schema violation should block with json_schema, got %+v", f)
	}
}

func TestCheckJSONSchemaWrongType(t *testing.T) {
	g := newJSONGuard()
	schema := []byte(`{"type":"object","properties":{"age":{"type":"integer"}},"required":["age"]}`)
	f := g.CheckJSONOutput(`{"age":"not a number"}`, schema)
	if !f.Blocked || f.Rule != "json_schema" {
		t.Fatalf("type mismatch should block, got %+v", f)
	}
}

func TestCheckJSONSchemaMalformedSchemaDoesNotBlock(t *testing.T) {
	g := newJSONGuard()
	// A broken client schema is a request problem; valid JSON output should pass.
	f := g.CheckJSONOutput(`{"ok":true}`, []byte(`{"type": 123 invalid`))
	if f.Blocked {
		t.Fatalf("malformed schema must not block valid JSON, got %+v", f)
	}
}

func TestActiveIncludesJSONValidation(t *testing.T) {
	g := New(Config{Enabled: true, ValidateJSONOutput: true})
	if !g.Active() {
		t.Fatal("guard with only JSON validation should be Active")
	}
}

func TestRepairJSONCodeFence(t *testing.T) {
	in := "```json\n{\"ok\":true}\n```"
	got, changed := RepairJSON(in)
	if !changed || got != `{"ok":true}` {
		t.Fatalf("want unfenced JSON, got %q changed=%v", got, changed)
	}
}

func TestRepairJSONBareFence(t *testing.T) {
	in := "```\n[1,2,3]\n```"
	got, changed := RepairJSON(in)
	if !changed || got != "[1,2,3]" {
		t.Fatalf("want unfenced array, got %q changed=%v", got, changed)
	}
}

func TestRepairJSONSurroundingProse(t *testing.T) {
	in := `Sure! Here is your answer: {"name":"Ada","age":36} Hope that helps.`
	got, changed := RepairJSON(in)
	if !changed || got != `{"name":"Ada","age":36}` {
		t.Fatalf("want extracted object, got %q changed=%v", got, changed)
	}
}

func TestRepairJSONNestedAndStrings(t *testing.T) {
	in := "prefix {\"a\":{\"b\":[1,2]},\"c\":\"has } brace\"} suffix"
	got, changed := RepairJSON(in)
	want := `{"a":{"b":[1,2]},"c":"has } brace"}`
	if !changed || got != want {
		t.Fatalf("want %q, got %q changed=%v", want, got, changed)
	}
}

func TestRepairJSONAlreadyClean(t *testing.T) {
	in := `{"ok":true}`
	if got, changed := RepairJSON(in); changed || got != in {
		t.Fatalf("clean JSON should not change, got %q changed=%v", got, changed)
	}
}

func TestRepairJSONNotJSON(t *testing.T) {
	in := "there is no json here at all"
	if _, changed := RepairJSON(in); changed {
		t.Fatal("non-JSON text should not report a change")
	}
}

func TestRepairThenValidate(t *testing.T) {
	g := newJSONGuard()
	fixed, changed := RepairJSON("```json\n{\"ok\":true}\n```")
	if !changed {
		t.Fatal("expected repair")
	}
	if f := g.CheckJSONOutput(fixed, nil); f.Blocked {
		t.Fatalf("repaired JSON should validate, got %+v", f)
	}
}
