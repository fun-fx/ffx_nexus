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

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConf  `json:"generationConfig,omitempty"`
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
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if gr.SystemInstruction == nil {
				gr.SystemInstruction = &geminiContent{}
			}
			gr.SystemInstruction.Parts = append(gr.SystemInstruction.Parts, geminiPart{Text: m.Content})
		case "assistant":
			gr.Contents = append(gr.Contents, geminiContent{Role: "model", Parts: []geminiPart{{Text: m.Content}}})
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

func (gr geminiResponse) text() string {
	var b strings.Builder
	if len(gr.Candidates) > 0 {
		for _, p := range gr.Candidates[0].Content.Parts {
			b.WriteString(p.Text)
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
	finish := ""
	if len(gres.Candidates) > 0 {
		finish = mapGeminiFinish(gres.Candidates[0].FinishReason)
	}
	return &gateway.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().Unix()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []gateway.Choice{{
			Index:        0,
			Message:      gateway.Message{Role: "assistant", Content: gres.text()},
			FinishReason: finish,
		}},
		Usage: gateway.Usage{
			PromptTokens:     gres.UsageMetadata.PromptTokenCount,
			CompletionTokens: gres.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gres.UsageMetadata.TotalTokenCount,
		},
	}, nil
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
			text := gres.text()
			finish := ""
			if len(gres.Candidates) > 0 {
				finish = mapGeminiFinish(gres.Candidates[0].FinishReason)
			}
			chunk := &gateway.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []gateway.ChunkChoice{{Index: 0, Delta: gateway.Delta{Content: text}, FinishReason: finish}},
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
