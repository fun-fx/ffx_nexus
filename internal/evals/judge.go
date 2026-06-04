package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// SLMJudge scores response quality using a small local language model exposed
// via an OpenAI-compatible /v1/chat/completions endpoint (e.g. Ollama, vLLM).
// Running the judge locally keeps trace content on-prem (data privacy) and
// avoids per-eval cost. It is expensive relative to heuristics, so the Worker
// gates it behind a sample rate.
type SLMJudge struct {
	baseURL string // e.g. http://localhost:11434/v1
	model   string // e.g. "llama3.2:3b"
	apiKey  string // optional
	hc      *http.Client
}

// JudgeConfig configures the SLM judge.
type JudgeConfig struct {
	BaseURL string
	Model   string
	APIKey  string
	Timeout time.Duration
}

// NewSLMJudge builds a judge. Returns nil if BaseURL or Model is empty (judge
// disabled).
func NewSLMJudge(cfg JudgeConfig) *SLMJudge {
	if cfg.BaseURL == "" || cfg.Model == "" {
		return nil
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Second
	}
	return &SLMJudge{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		apiKey:  cfg.APIKey,
		hc:      &http.Client{Timeout: cfg.Timeout},
	}
}

// Name implements Evaluator.
func (j *SLMJudge) Name() string { return "slm_judge" }

const judgeSystemPrompt = `You are a strict evaluator of AI assistant responses. ` +
	`Given a user prompt and the assistant's answer, rate the answer's overall ` +
	`quality (correctness, relevance, helpfulness) from 0.0 to 1.0. ` +
	`Respond with ONLY a compact JSON object: {"score": <float 0..1>, "rationale": "<one sentence>"}. ` +
	`Do not include any other text.`

type judgeChatReq struct {
	Model       string         `json:"model"`
	Messages    []judgeChatMsg `json:"messages"`
	Temperature float64        `json:"temperature"`
	MaxTokens   int            `json:"max_tokens"`
	Stream      bool           `json:"stream"`
}

type judgeChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type judgeChatResp struct {
	Choices []struct {
		Message judgeChatMsg `json:"message"`
	} `json:"choices"`
}

type judgeVerdict struct {
	Score     float64 `json:"score"`
	Rationale string  `json:"rationale"`
}

var reJSONObj = regexp.MustCompile(`\{[\s\S]*\}`)

// Evaluate implements Evaluator. Skips traces with no captured content.
func (j *SLMJudge) Evaluate(ctx context.Context, t observability.Trace) ([]Score, error) {
	if t.InputMessages == "" || t.OutputMessages == "" {
		return nil, nil
	}

	userContent := fmt.Sprintf("User prompt:\n%s\n\nAssistant answer:\n%s",
		truncate(t.InputMessages, 4000), truncate(t.OutputMessages, 4000))

	body, _ := json.Marshal(judgeChatReq{
		Model: j.model,
		Messages: []judgeChatMsg{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature: 0,
		MaxTokens:   200,
		Stream:      false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if j.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+j.apiKey)
	}

	resp, err := j.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("judge upstream %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var cr judgeChatResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("judge returned no choices")
	}

	verdict, err := parseVerdict(cr.Choices[0].Message.Content)
	if err != nil {
		return nil, err
	}

	score := clamp01(verdict.Score)
	return []Score{{
		TraceID:    t.TraceID,
		Timestamp:  time.Now().UTC(),
		Evaluator:  "slm_judge",
		Metric:     "quality",
		Score:      score,
		Passed:     score >= 0.6,
		Rationale:  verdict.Rationale,
		JudgeModel: j.model,
	}}, nil
}

// parseVerdict extracts the JSON verdict from a model reply, tolerating extra
// prose around the JSON object.
func parseVerdict(content string) (judgeVerdict, error) {
	var v judgeVerdict
	jsonStr := content
	if !strings.HasPrefix(strings.TrimSpace(content), "{") {
		if m := reJSONObj.FindString(content); m != "" {
			jsonStr = m
		}
	}
	if err := json.Unmarshal([]byte(jsonStr), &v); err != nil {
		return v, fmt.Errorf("parse judge verdict: %w (raw: %s)", err, truncate(content, 200))
	}
	return v, nil
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
