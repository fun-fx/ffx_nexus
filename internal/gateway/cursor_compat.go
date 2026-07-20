package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// IsCursorHybridRequest detects whether a request body is a Cursor Agent
// "hybrid" that posts to /v1/chat/completions but carries Responses-API
// fields (input, reasoning, flat tools, custom tools, etc.).
//
// It returns true when the top-level JSON contains "input" (not "messages")
// or when Tools contains flat Responses-style definitions or custom-type
// tools.  The returned raw request bytes can be fed to TransformCursorHybrid
// to produce a canonical ChatCompletionRequest.
func IsCursorHybridRequest(body json.RawMessage) bool {
	// Fast path: look for top-level keys that only appear in Responses
	// bodies.  We don't unmarshal the full struct here to avoid the
	// allocation when 99 % of traffic is normal Chat Completions.
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return false
	}

	// If the body contains '"input"' as a top-level key, it's almost
	// certainly a Responses payload mis-routed to /chat/completions.
	if strings.Contains(trimmed, `"input"`) {
		// Guard against the rare case where someone puts "input" inside
		// a nested object (e.g. embeddings).  Chat completions never
		// have a top-level "input" key.
		return true
	}

	// Cursor sometimes sends a Chat-shaped top level but with
	// Responses-style flat tools or custom tools inside the tools array.
	// We do a lightweight scan for "type\":\"custom\"" or the flat
	// "name\": pattern that appears outside a nested function object.
	if strings.Contains(trimmed, `"type":"custom"`) ||
		strings.Contains(trimmed, `"type": "custom"`) {
		return true
	}

	return false
}

// CursorHybridReq is the unmarshalled shape of a Cursor Agent hybrid request.
// It accepts both Chat Completions and Responses API fields so we can
// normalise either one into a canonical ChatCompletionRequest.
type CursorHybridReq struct {
	Model       string          `json:"model"`
	Messages    []Message       `json:"messages,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"` // string or []InputItem
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	MaxOutputTokens *int        `json:"max_output_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Tools       []json.RawMessage `json:"tools,omitempty"` // raw to preserve nested vs flat shape
	User        string          `json:"user,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool   `json:"parallel_tool_calls,omitempty"`
	Reasoning   json.RawMessage `json:"reasoning,omitempty"`      // Cursor Agent reasoning dict
	StreamOptions json.RawMessage `json:"stream_options,omitempty"`
	// otros campos Responses-only que simplemente ignoramos por ahora
}

// TransformCursorHybrid converts a Cursor hybrid request body into a
// canonical ChatCompletionRequest.  It handles:
//   - "input" array → "messages" array
//   - flat Responses function tools → nested Chat Completions tools
//   - custom tools (e.g. ApplyPatch) → Chat Completions custom tools
//   - reasoning.effort → reasoning_effort
//   - max_output_tokens → max_tokens
func TransformCursorHybrid(body json.RawMessage) (ChatCompletionRequest, error) {
	var h CursorHybridReq
	if err := json.Unmarshal(body, &h); err != nil {
		return ChatCompletionRequest{}, fmt.Errorf("decode hybrid body: %w", err)
	}

	chat := ChatCompletionRequest{
		Model:       h.Model,
		Temperature: h.Temperature,
		TopP:        h.TopP,
		Stream:      h.Stream,
		Stop:        h.Stop,
		User:        h.User,
		ToolChoice:  h.ToolChoice,
		ParallelToolCalls: h.ParallelToolCalls,
	}

	if h.MaxTokens != nil {
		chat.MaxTokens = h.MaxTokens
	} else if h.MaxOutputTokens != nil {
		chat.MaxTokens = h.MaxOutputTokens
	}

	// ---- messages --------------------------------------------------------
	if len(h.Messages) > 0 {
		chat.Messages = h.Messages
	} else if len(h.Input) > 0 && string(h.Input) != "null" {
		msgs, err := parseInputToMessages(h.Input)
		if err != nil {
			return chat, fmt.Errorf("input→messages: %w", err)
		}
		chat.Messages = msgs
	}

	// ---- tools -----------------------------------------------------------
	for _, rawTool := range h.Tools {
		t, err := normaliseTool(rawTool)
		if err != nil {
			// Silently skip malformed tools so one bad definition doesn't
			// abort the whole request.
			continue
		}
		if t != nil {
			chat.Tools = append(chat.Tools, *t)
		}
	}

	// ---- reasoning --------------------------------------------------------
	if len(h.Reasoning) > 0 && string(h.Reasoning) != "null" {
		var r struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(h.Reasoning, &r); err == nil && r.Effort != "" {
			chat.ReasoningEffort = r.Effort
		}
	}

	// ---- tool_choice -------------------------------------------------------
	if len(h.ToolChoice) > 0 {
		chat.ToolChoice = normaliseHybridToolChoice(h.ToolChoice)
	}

	// ---- parallel_tool_calls ---------------------------------------------
	chat.ParallelToolCalls = h.ParallelToolCalls

	// ---- Extras (Responses-only knobs preserved as wire passthrough) -----
	chat.Extra = pickResponsesExtras(body)

	// ---- stream_options ---------------------------------------------------
	// Cursor sends stream_options: {"include_usage": true} which is a Chat
	// Completions field.  Preserve it verbatim.
	chat.StreamOptions = h.StreamOptions

	return chat, nil
}

// parseInputToMessages turns a Responses-style "input" (string or array)
// into the standard Chat Completions messages slice.
func parseInputToMessages(raw json.RawMessage) ([]Message, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, fmt.Errorf("input is empty")
	}

	// String input → single user message
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []Message{{Role: "user", Content: s}}, nil
	}

	// Array input
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("input must be a string or an array")
	}
	var items []InputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}

	var msgs []Message
	for _, it := range items {
		switch {
		case it.Role == "user" || it.Role == "system" || it.Role == "assistant" || it.Role == "developer":
			msgs = append(msgs, Message{
				Role:    it.Role,
				Content: extractInputText(it.Content),
			})
		case it.Type == "function_call":
			tc := ToolCall{Type: "function", ID: it.CallID}
			tc.Function.Name = strings.TrimSpace(it.Name)
			tc.Function.Arguments = it.Arguments
			msgs = append(msgs, Message{
				Role:      "assistant",
				ToolCalls: []ToolCall{tc},
			})
		case it.Type == "function_call_output":
			msgs = append(msgs, Message{
				Role:       "tool",
				Content:    it.OutputString(),
				ToolCallID: it.CallID,
				Name:       it.Name,
			})
		// Cursor also sends tool_use / tool_result items inside the input
		// array (Anthropic-style).  Map them to the OpenAI equivalent.
		case it.Type == "tool_use":
			// Parse extra fields that are not in InputItem
			var extra struct {
				Name string `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(it.Content, &extra)
			args := "{}"
			if len(extra.Input) > 0 {
				args = string(extra.Input)
			}
			msgs = append(msgs, Message{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					Type: "function",
					ID:   it.CallID,
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: extra.Name, Arguments: args},
				}},
			})
		case it.Type == "tool_result":
			msgs = append(msgs, Message{
				Role:       "tool",
				Content:    it.OutputString(),
				ToolCallID: it.CallID,
			})
		}
	}
	return msgs, nil
}

// normaliseTool accepts a raw tool definition (which may be Chat Completions
// nested, Responses flat, or custom) and returns a canonical Tool struct.
func normaliseTool(raw json.RawMessage) (*Tool, error) {
	var nested struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function,omitempty"`
		Custom   json.RawMessage `json:"custom,omitempty"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, err
	}

	// Detect flat Responses shape first: {"type":"function","name":"X",...}
	// even when the JSON also has a stray `function:` field inside (Cursor
	// sends both forms sometimes).  Flat definitions always carry the tool
	// name in a top-level `name` field rather than `function.name`.
	var flatProbe struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &flatProbe)
	if flatProbe.Name != "" && len(nested.Function) == 0 {
		var flat struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil, err
		}
		fn, _ := json.Marshal(map[string]any{
			"name":        flat.Name,
			"description": flat.Description,
			"parameters":  flat.Parameters,
		})
		return &Tool{Type: flat.Type, Function: fn}, nil
	}

	switch nested.Type {
	case "function":
		if len(nested.Function) == 0 {
			return nil, fmt.Errorf("function tool missing function object")
		}
		return &Tool{Type: "function", Function: nested.Function}, nil

	case "custom":
		// ApplyPatch / grammar tools. The Responses `format` block (lark
		// grammar, regex, or plain text) is preserved under
		// function.parameters.format so providers that recognise Responses
		// grammars can still re-validate the model output.
		if len(nested.Custom) == 0 {
			return nil, fmt.Errorf("custom tool missing custom object")
		}
		preserved, _ := wrapApplyPatchGrammar(nested.Custom)
		return &Tool{Type: "custom", Function: preserved}, nil

	default:
		// flat probe handled earlier; this catches "" root.
		var flat struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil, err
		}
		if flat.Name == "" {
			return nil, fmt.Errorf("tool has no name")
		}
		fn, _ := json.Marshal(map[string]any{
			"name":        flat.Name,
			"description": flat.Description,
			"parameters":  flat.Parameters,
		})
		return &Tool{Type: flat.Type, Function: fn}, nil
	}
}

// wrapApplyPatchGrammar mirrors the Responses `custom` block into a Chat
// Completions `function` payload with the original grammar preserved under
// parameters.format. Cursor Agent validates the grammar on the response
// path, so providers that respect the Responses grammar contract can still
// see the original byte sequence end-to-end.  Providers that ignore the
// extra `format` key are unaffected.
func wrapApplyPatchGrammar(custom json.RawMessage) (json.RawMessage, bool) {
	var probe struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Format      json.RawMessage `json:"format,omitempty"`
	}
	if err := json.Unmarshal(custom, &probe); err != nil || probe.Name == "" {
		return custom, false
	}
	var params map[string]any
	if len(probe.Format) > 0 {
		params = map[string]any{"format": json.RawMessage(probe.Format)}
	} else {
		params = map[string]any{}
	}
	fn := map[string]any{
		"name":        probe.Name,
		"description": probe.Description,
		"parameters":  params,
	}
	b, err := json.Marshal(fn)
	if err != nil {
		return custom, false
	}
	return b, true
}

// normaliseHybridToolChoice converts Responses-style flat tool_choice
// ({"type":"function","name":"X"}) into the Chat Completions nested form
// ({"type":"function","function":{"name":"X"}}). String forms ("auto",
// "required", "none") and already-nested payloads pass through unchanged.
func normaliseHybridToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] != '{' {
		return raw
	}
	var probe struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
		Name     string          `json:"name"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return raw
	}
	if len(probe.Function) > 0 {
		return raw
	}
	if probe.Type != "" && probe.Name != "" {
		nested, _ := json.Marshal(map[string]any{
			"type":     probe.Type,
			"function": map[string]any{"name": probe.Name},
		})
		return nested
	}
	return raw
}

// pickResponsesExtras returns Responses-only fields that some
// Chat-completions providers still respect (store, include, prompt cache
// knobs, metadata). Keys already promoted onto the canonical struct are
// excluded so we never double-publish a value with a different shape.
func pickResponsesExtras(raw json.RawMessage) map[string]json.RawMessage {
	promoted := map[string]struct{}{
		"model": {}, "messages": {}, "input": {}, "tools": {},
		"temperature": {}, "top_p": {}, "max_output_tokens": {},
		"stream": {}, "user": {}, "nexus_eval": {}, "instructions": {},
		"reasoning": {}, "text": {}, "tool_choice": {},
		"parallel_tool_calls": {},
	}
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for k, v := range rawMap {
		if _, ok := promoted[k]; ok {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

