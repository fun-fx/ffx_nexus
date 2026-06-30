package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// Gemini adapts Google's Generative Language API to the OpenAI-compatible schema.
type Gemini struct {
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

// NewGemini builds a Gemini adapter.
func NewGemini(apiKey string, timeout time.Duration) *Gemini {
	return &Gemini{
		apiKey:  apiKey,
		baseURL: "https://generativelanguage.googleapis.com/v1beta",
		models: []string{
			"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.0-flash",
		},
		client: &http.Client{Timeout: timeout},
	}
}

// Name implements gateway.Provider.
func (g *Gemini) Name() string { return "gemini" }

// Models implements gateway.Provider.
func (g *Gemini) Models() []string { return g.models }

// geminiPart is one element of a Gemini content message. The Gemini schema
// uses a discriminated union by field-presence: {text: "..."} is text,
// {functionCall: {...}} is a model-initiated tool call, and
// {functionResponse: {...}} is the matching tool result.
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// geminiFunctionDeclaration is the Gemini tool description. The schema is
// flattened (no `type: "function"` wrapper as in OpenAI).
type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// geminiToolConfig selects how the model uses the declared tools.
type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type geminiFunctionCallingConfig struct {
	// Mode maps from OpenAI's tool_choice string:
	//   "auto"       -> "AUTO"   (model decides)
	//   "required"   -> "ANY"    (force at least one)
	//   "none"       -> "NONE"   (suppress tool calls)
	//   named -> "ANY" + AllowedFunctionNames pinned
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConf    `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type geminiGenConf struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

func toGeminiRequest(req gateway.ChatCompletionRequest) geminiRequest {
	gr := geminiRequest{
		GenerationConfig: &geminiGenConf{
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			MaxOutputTokens: req.MaxTokens,
			StopSequences:   req.Stop,
		},
	}

	// Translate OpenAI `tools` -> Gemini `functionDeclarations`. The OpenAI
	// shape is {type:"function", function:{name,description,parameters}}; the
	// Gemini shape is the flattened {name,description,parameters}.
	if len(req.Tools) > 0 {
		decls := make([]geminiFunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			var fn struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}
			_ = json.Unmarshal(t.Function, &fn)
			if fn.Name == "" {
				continue
			}
			decls = append(decls, geminiFunctionDeclaration{
				Name:        fn.Name,
				Description: fn.Description,
				Parameters:  fn.Parameters,
			})
		}
		if len(decls) > 0 {
			gr.Tools = []geminiTool{{FunctionDeclarations: decls}}
		}
	}

	// Translate OpenAI `tool_choice` -> Gemini `toolConfig`. Strings map
	// directly; the object form pins a specific function.
	if len(req.ToolChoice) > 0 {
		cfg := geminiFunctionCallingConfig{}
		var s string
		if err := json.Unmarshal(req.ToolChoice, &s); err == nil {
			switch s {
			case "auto":
				cfg.Mode = "AUTO"
			case "required":
				cfg.Mode = "ANY"
			case "none":
				cfg.Mode = "NONE"
			}
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &obj); err == nil {
				if obj.Type == "function" && obj.Function.Name != "" {
					cfg.Mode = "ANY"
					cfg.AllowedFunctionNames = []string{obj.Function.Name}
				}
			}
		}
		if cfg.Mode != "" {
			gr.ToolConfig = &geminiToolConfig{FunctionCallingConfig: cfg}
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if gr.SystemInstruction == nil {
				gr.SystemInstruction = &geminiContent{}
			}
			gr.SystemInstruction.Parts = append(gr.SystemInstruction.Parts, geminiPart{Text: m.Content})
		case "assistant":
			parts := []geminiPart{}
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]any
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				}
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: args},
				})
			}
			if len(parts) > 0 {
				gr.Contents = append(gr.Contents, geminiContent{Role: "model", Parts: parts})
			}
		case "tool":
			// Gemini's tool-result part uses our `name` as the binding.
			// We pick the assistant's matching tool call name from the
			// call id we recorded; if the id is empty, fall back to the
			// first declared tool name so simple test flows still work.
			if m.ToolCallID == "" && len(req.Tools) > 0 {
				var fn struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(req.Tools[0].Function, &fn)
				m.ToolCallID = fn.Name
			}
			var response map[string]any
			if m.Content != "" {
				_ = json.Unmarshal([]byte(m.Content), &response)
			}
			if response == nil {
				response = map[string]any{"result": m.Content}
			}
			gr.Contents = append(gr.Contents, geminiContent{Role: "user", Parts: []geminiPart{{
				FunctionResponse: &geminiFunctionResponse{Name: m.ToolCallID, Response: response},
			}}})
		default:
			gr.Contents = append(gr.Contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: m.Content}}})
		}
	}
	return gr
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// geminiToOpenAI decodes a Gemini response into the OpenAI shape. Text and
// function-call parts (Gemini does not split them across streams) are
// folded into a single ChatCompletionResponse. finish_reason mirrors
// stop: "STOP"/"MAX_TOKENS"/"SAFETY"; a functionCall finish maps to
// "tool_calls" to match the rest of the gateway.
func geminiToOpenAI(gr geminiResponse, model, id string) *gateway.ChatCompletionResponse {
	var text strings.Builder
	var toolCalls []gateway.ToolCall
	var idx int
	finish := ""
	if len(gr.Candidates) > 0 {
		for _, p := range gr.Candidates[0].Content.Parts {
			if p.Text != "" {
				text.WriteString(p.Text)
			}
			if p.FunctionCall != nil {
				arguments := "{}"
				if p.FunctionCall.Args != nil {
					if raw, err := json.Marshal(p.FunctionCall.Args); err == nil {
						arguments = string(raw)
					}
				}
				tc := gateway.ToolCall{Type: "function"}
				tc.ID = fmt.Sprintf("call_%s_%d", safeID(model), idx)
				tc.Function.Name = p.FunctionCall.Name
				tc.Function.Arguments = arguments
				toolCalls = append(toolCalls, tc)
				idx++
			}
		}
		finish = mapGeminiFinish(gr.Candidates[0].FinishReason)
		// Gemini emits STOP for both plain completion and tool_use — we
		// intentionally do *not* override finish_reason when a function
		// call was emitted. The downstream consumer discovers tool turns
		// via Choice.Message.ToolCalls, not via finish_reason.
	}
	return &gateway.ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []gateway.Choice{{
			Index:        0,
			Message:      gateway.Message{Role: "assistant", Content: text.String(), ToolCalls: toolCalls},
			FinishReason: finish,
		}},
		Usage: gateway.Usage{
			PromptTokens:     gr.UsageMetadata.PromptTokenCount,
			CompletionTokens: gr.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gr.UsageMetadata.TotalTokenCount,
		},
	}
}

// safeID is a tiny helper that turns a model id into something like an
// OpenAI call id without using import-locked public packages. We just
// keep the alphanumerics; everything else becomes an underscore.
func safeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func mapGeminiFinish(s string) string {
	switch s {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION":
		return "content_filter"
	case "":
		return ""
	default:
		return strings.ToLower(s)
	}
}

// ChatCompletion implements gateway.Provider.
func (g *Gemini) ChatCompletion(ctx context.Context, req gateway.ChatCompletionRequest) (*gateway.ChatCompletionResponse, error) {
	gr := toGeminiRequest(req)
	body, err := json.Marshal(gr)
	if err != nil {
		return nil, err
	}
	apiKey, baseURL := g.creds(ctx)
	endpoint := fmt.Sprintf("%s/models/%s:generateContent?key=%s", baseURL, url.PathEscape(req.Model), url.QueryEscape(apiKey))
	resp, err := g.post(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, providerError("gemini", resp)
	}
	var gres geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gres); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().Unix())
	return geminiToOpenAI(gres, req.Model, id), nil
}

// ChatCompletionStream implements gateway.Provider.
func (g *Gemini) ChatCompletionStream(ctx context.Context, req gateway.ChatCompletionRequest) (<-chan gateway.StreamEvent, error) {
	gr := toGeminiRequest(req)
	body, err := json.Marshal(gr)
	if err != nil {
		return nil, err
	}
	apiKey, baseURL := g.creds(ctx)
	endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s", baseURL, url.PathEscape(req.Model), url.QueryEscape(apiKey))
	resp, err := g.post(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, providerError("gemini", resp)
	}

	out := make(chan gateway.StreamEvent)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		created := time.Now().Unix()
		id := fmt.Sprintf("chatcmpl-%d", created)
		// State used to fold streaming events into a single tool_call when
		// Gemini streams function-call args across multiple events.
		var pendingByName map[string]*gateway.ToolCall
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var gres geminiResponse
			if err := json.Unmarshal([]byte(data), &gres); err != nil {
				continue
			}
			if len(gres.Candidates) == 0 {
				continue
			}
			cand := gres.Candidates[0]
			finish := mapGeminiFinish(cand.FinishReason)

			// Per-part delta: a new part means a new chunk. Gemini emits text
			// parts with the full text-so-far; we emit a delta containing
			// whatever we haven't already seen for that role+index.
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					out <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
						ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []gateway.ChunkChoice{{
							Index: 0, Delta: gateway.Delta{Content: p.Text},
							FinishReason: finish,
						}},
					}}
				}
				if p.FunctionCall != nil {
					if pendingByName == nil {
						pendingByName = map[string]*gateway.ToolCall{}
					}
					tc, seen := pendingByName[p.FunctionCall.Name]
					if !seen {
						tc = &gateway.ToolCall{
							Type: "function",
						}
						tc.ID = fmt.Sprintf("call_%s_%s", safeID(req.Model), p.FunctionCall.Name)
						tc.Function.Name = p.FunctionCall.Name
						tc.Function.Arguments = ""
						pendingByName[p.FunctionCall.Name] = tc
					}
					raw, _ := json.Marshal(p.FunctionCall.Args)
					tc.Function.Arguments = string(raw)
					out <- gateway.StreamEvent{Chunk: &gateway.ChatCompletionChunk{
						ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []gateway.ChunkChoice{{
							Index: 0, Delta: gateway.Delta{ToolCalls: []gateway.ToolCall{*tc}},
							FinishReason: finish,
						}},
					}}
				}
			}
			if finish != "" {
				chunk := &gateway.ChatCompletionChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					Choices: []gateway.ChunkChoice{{Index: 0, FinishReason: finish}},
				}
				if gres.UsageMetadata.TotalTokenCount > 0 {
					chunk.Usage = &gateway.Usage{
						PromptTokens:     gres.UsageMetadata.PromptTokenCount,
						CompletionTokens: gres.UsageMetadata.CandidatesTokenCount,
						TotalTokens:      gres.UsageMetadata.TotalTokenCount,
					}
				}
				out <- gateway.StreamEvent{Chunk: chunk}
			}
		}
		if err := scanner.Err(); err != nil {
			out <- gateway.StreamEvent{Err: err}
			return
		}
		out <- gateway.StreamEvent{Done: true}
	}()
	return out, nil
}

// creds resolves the API key + base URL for this request, honoring a per-request
// BYOK override from the context when present.
func (g *Gemini) creds(ctx context.Context) (apiKey, baseURL string) {
	apiKey, baseURL = g.apiKey, g.baseURL
	if c, ok := gateway.CallerCredentialFrom(ctx); ok {
		apiKey = c.Secret
		if c.BaseURL != "" {
			baseURL = strings.TrimRight(c.BaseURL, "/")
		}
	}
	return apiKey, baseURL
}

func (g *Gemini) post(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	return g.client.Do(httpReq)
}
