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
		if len(nested.Custom) == 0 {
			return nil, fmt.Errorf("custom tool missing custom object")
		}
		return &Tool{Type: "custom", Function: nested.Custom}, nil

	default:
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
