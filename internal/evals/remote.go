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
	apiKey  string // optional bearer token; never logged
	hc      *http.Client
}

// RemoteConfig configures the remote eval evaluator.
type RemoteConfig struct {
	BaseURL string        // e.g. http://localhost:8200 (empty disables)
	Metrics []string      // metric ids requested, e.g. ["answer_relevancy","toxicity"]
	Timeout time.Duration // default 30s when zero
	// APIKey is an optional bearer token sent as "Authorization: Bearer ...".
	// Empty keeps the legacy behaviour (sidecars that don't require auth
	// skip the header altogether). Populated by the worker's secret
	// resolver when the per-profile key source resolves to a string.
	APIKey string
}

// NewRemoteEvaluator builds a remote evaluator. Returns nil when BaseURL is
// empty (feature disabled). PR #135 widens the http client with
// IdleConnTimeout and MaxIdleConnsPerHost so consecutive traces reuse
// the TCP/TLS connection (HTTP keep-alive) — before this change every
// remote-eval call paid a fresh handshake, which on a 30 ms RTT lan
// is the difference between a healthy eval pipeline and a slow one.
func NewRemoteEvaluator(cfg RemoteConfig) *RemoteEvaluator {
	if cfg.BaseURL == "" {
		return nil
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: 500 * time.Millisecond,
	}
	return &RemoteEvaluator{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		metrics: cfg.Metrics,
		apiKey:  cfg.APIKey,
		hc:      &http.Client{Timeout: cfg.Timeout, Transport: transport},
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

	contexts := parseRetrievalContexts(t.RetrievalContexts)
	metrics := mergeContextMetrics(r.metrics, contexts)

	body, _ := json.Marshal(remoteRequest{
		TraceID:   t.TraceID,
		Model:     t.RequestModel,
		Input:     truncate(extractPrompt(t.InputMessages), 8000),
		Output:    truncate(t.OutputMessages, 8000),
		Contexts:  contexts,
		Reference: t.EvalReference,
		Metrics:   metrics,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/evaluate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.apiKey)
	}

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
