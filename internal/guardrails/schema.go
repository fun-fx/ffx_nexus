package guardrails

import (
	"bytes"
	"encoding/json"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// CheckJSONOutput validates that output is well-formed JSON and, when a JSON
// Schema is supplied, that it conforms to that schema. It only runs when JSON
// output validation is enabled; otherwise it is a no-op.
//
// schema may be nil (json_object mode): then only structural JSON validity is
// checked. A non-nil schema (json_schema mode) is additionally enforced.
func (g *Guard) CheckJSONOutput(output string, schema []byte) Finding {
	if g == nil || !g.cfg.ValidateJSONOutput {
		return allow
	}
	if output == "" {
		return Finding{Blocked: true, Rule: "json_output",
			Reason: "response is empty but a JSON response_format was requested"}
	}

	var doc interface{}
	if err := json.Unmarshal([]byte(output), &doc); err != nil {
		return Finding{Blocked: true, Rule: "json_output",
			Reason: "response is not valid JSON: " + err.Error()}
	}

	if len(schema) == 0 {
		return allow
	}

	compiled, err := compileSchema(schema)
	if err != nil {
		// A malformed client-supplied schema is a request problem, not an output
		// violation; do not block the response on it.
		return allow
	}
	if err := compiled.Validate(doc); err != nil {
		return Finding{Blocked: true, Rule: "json_schema",
			Reason: "response does not conform to the requested JSON schema: " + summarizeSchemaErr(err)}
	}
	return allow
}

// compileSchema builds a validator from raw JSON Schema bytes.
func compileSchema(schema []byte) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", bytes.NewReader(schema)); err != nil {
		return nil, err
	}
	return c.Compile("schema.json")
}

func summarizeSchemaErr(err error) string {
	msg := err.Error()
	const max = 300
	if len(msg) > max {
		return msg[:max] + "..."
	}
	return msg
}
