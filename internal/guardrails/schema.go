package guardrails

import (
	"bytes"
	"encoding/json"
	"strings"

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

// RepairJSON attempts a free, local recovery of JSON that a model wrapped in a
// markdown code fence or surrounded with prose ("Sure, here you go: {...}"). It
// strips a leading/trailing ``` fence and extracts the outermost JSON object or
// array. It returns the candidate and whether it differs from the input; it does
// not validate, so callers must re-run CheckJSONOutput on the result.
//
// This runs only on the failure path (after validation rejects a response), so
// the cost is a few bounded string scans and never touches the success path.
func RepairJSON(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	candidate := stripCodeFence(trimmed)
	if ext, ok := extractBracketed(candidate); ok {
		candidate = ext
	}
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || candidate == trimmed {
		return s, false
	}
	return candidate, true
}

// stripCodeFence removes a surrounding ```/```json markdown fence if present.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (``` or ```json) up to the first newline.
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		return s
	}
	// Drop a trailing closing fence.
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// extractBracketed returns the substring from the first '{' or '[' to its
// matching close, accounting for nested brackets and string literals.
func extractBracketed(s string) (string, bool) {
	start := -1
	var open, close byte
	for i := 0; i < len(s); i++ {
		if s[i] == '{' {
			start, open, close = i, '{', '}'
			break
		}
		if s[i] == '[' {
			start, open, close = i, '[', ']'
			break
		}
	}
	if start == -1 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func summarizeSchemaErr(err error) string {
	msg := err.Error()
	const max = 300
	if len(msg) > max {
		return msg[:max] + "..."
	}
	return msg
}
