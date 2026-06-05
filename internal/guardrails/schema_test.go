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
