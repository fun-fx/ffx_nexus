package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// RemoteEvaluator delegates evaluation to an external HTTP eval service — a
// Python sidecar running mature eval libraries (DeepEval, RAGAS). It implements
// Evaluator and is treated as an expensive judge by the Worker, so it is
// sample-gated and never sits on the request hot path.
//
// Failure isolation: any transport/decode error is returned to the Worker,
// which logs it and continues with the remaining evaluators. The Python service
// being slow or down therefore degrades gracefully to the Go heuristics and
// never affects the gateway response or routing availability.
type RemoteEvaluator struct {
	baseURL string
	metrics []string
	hc      *http.Client
}

// RemoteConfig configures the remote eval evaluator.
type RemoteConfig struct {
	BaseURL string   // e.g. http://localhost:8200 (empty disables)
	Metrics []string // metric ids requested, e.g. ["answer_relevancy","toxicity"]
	Timeout time.Duration
}

// NewRemoteEvaluator builds a remote evaluator. Returns nil when BaseURL is
// empty (feature disabled).
func NewRemoteEvaluator(cfg RemoteConfig) *RemoteEvaluator {
	if cfg.BaseURL == "" {
		return nil
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &RemoteEvaluator{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		metrics: cfg.Metrics,
		hc:      &http.Client{Timeout: cfg.Timeout},
	}
}

// Name implements Evaluator.
func (r *RemoteEvaluator) Name() string { return "remote_eval" }

type remoteRequest struct {
	TraceID   string   `json:"trace_id"`
	Model     string   `json:"model"`
	Input     string   `json:"input"`
	Output    string   `json:"output"`
	Contexts  []string `json:"contexts,omitempty"`
	Reference string   `json:"reference,omitempty"`
	Metrics   []string `json:"metrics,omitempty"`
}

type remoteScore struct {
	Evaluator  string  `json:"evaluator"`
	Metric     string  `json:"metric"`
	Score      float64 `json:"score"`
	Passed     bool    `json:"passed"`
	Rationale  string  `json:"rationale"`
	JudgeModel string  `json:"judge_model"`
}

type remoteResponse struct {
	Scores []remoteScore `json:"scores"`
}

// Evaluate implements Evaluator. Skips traces without captured content.
func (r *RemoteEvaluator) Evaluate(ctx context.Context, t observability.Trace) ([]Score, error) {
	if t.InputMessages == "" || t.OutputMessages == "" {
		return nil, nil
	}

	body, _ := json.Marshal(remoteRequest{
		TraceID: t.TraceID,
		Model:   t.RequestModel,
		Input:   truncate(t.InputMessages, 8000),
		Output:  truncate(t.OutputMessages, 8000),
		Metrics: r.metrics,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eval service %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var rr remoteResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, err
	}

	out := make([]Score, 0, len(rr.Scores))
	for _, s := range rr.Scores {
		evaluator := s.Evaluator
		if evaluator == "" {
			evaluator = "remote_eval"
		}
		out = append(out, Score{
			TraceID:    t.TraceID,
			Timestamp:  time.Now().UTC(),
			Evaluator:  evaluator,
			Metric:     s.Metric,
			Score:      clamp01(s.Score),
			Passed:     s.Passed,
			Rationale:  s.Rationale,
			JudgeModel: s.JudgeModel,
		})
	}
	return out, nil
}
