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

// RemoteEvaluatorRequest is the wire payload sent to the Python sidecar.
// It is `EvaluateRequest` in eval-service except Go prepends profile
// overrides here so a profile-bound judge / embeddings endpoint can be
// substituted without a process restart (PR #136).
type RemoteEvaluatorRequest struct {
	TraceID    string   `json:"trace_id"`
	Model      string   `json:"model"`
	Input      string   `json:"input"`
	Output     string   `json:"output"`
	Contexts   []string `json:"contexts,omitempty"`
	Reference  string   `json:"reference,omitempty"`
	Metrics    []string `json:"metrics,omitempty"`
	JudgeURL   string   `json:"judge_url,omitempty"`
	JudgeModel string   `json:"judge_model,omitempty"`
	Threshold  float64  `json:"threshold,omitempty"`
}

type remoteBatchRequest struct {
	Items []RemoteEvaluatorRequest `json:"items"`
}

type remoteBatchResponse struct {
	ScoresByTrace map[string][]remoteScore `json:"scores_by_trace"`
}

// remoteScore is the inner reaction leg of the response payload used by
// /evaluate (single and batch).
type remoteScore struct {
	Evaluator  string  `json:"evaluator"`
	Metric     string  `json:"metric"`
	Score      float64 `json:"score"`
	Passed     bool    `json:"passed"`
	Rationale  string  `json:"rationale"`
	JudgeModel string  `json:"judge_model"`
}

// remoteResponse stays in place for /evaluate single-trace callers; the
// batched path uses remoteBatchResponse (above).
type remoteResponse struct {
	Scores []remoteScore `json:"scores"`
}

// remoteURL is the sidecar host: e.g. http://eval-service:8200. The
// path used at request time is either /evaluate (single) or
// /evaluate/batch (multi-trace). The sidecar supports both.
func (r *RemoteEvaluator) post(ctx context.Context, action string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+action, bytes.NewReader(body))
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
		return nil, fmt.Errorf("eval service %s %d: %s", action, resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// buildRequest fills the wire payload using trace data + per-profile
// overrides. Pulled out of Evaluate so batched calls reuse the same
// builder — keeps /evaluate and /evaluate/batch consistent on
// overrides, threshold, judge model etc.
func (r *RemoteEvaluator) buildRequest(t observability.Trace, ov *EvalOverride) RemoteEvaluatorRequest {
	contexts := parseRetrievalContexts(t.RetrievalContexts)
	metrics := mergeContextMetrics(r.metrics, contexts)
	req := RemoteEvaluatorRequest{
		TraceID:   t.TraceID,
		Model:     t.RequestModel,
		Input:     truncate(extractPrompt(t.InputMessages), 8000),
		Output:    truncate(t.OutputMessages, 8000),
		Contexts:  contexts,
		Reference: t.EvalReference,
		Metrics:   metrics,
	}
	if ov != nil {
		if ov.JudgeURL != "" {
			req.JudgeURL = ov.JudgeURL
			req.JudgeModel = ov.JudgeModel
		}
		if ov.Threshold > 0 {
			req.Threshold = ov.Threshold
		}
	}
	return req
}

// EvaluateBatch processes a TraceBatch with a single HTTP sidecar call.
// Returns a slice of Score slices keyed by the batch position so the
// runtime can splice them back into the per-trace fan-out. Errors
// from a single item fall through as zero scores for that trace — the
// caller (worker dispatch) keeps heuristic scores intact.
func (r *RemoteEvaluator) EvaluateBatch(ctx context.Context, batch TraceBatch, ov *EvalOverride) ([][]Score, error) {
	if len(batch.Traces) == 0 {
		return nil, nil
	}
	if len(batch.Traces) == 1 {
		// Don't pay the batch-overhead on singles; the same /evaluate
		// path Evaluate() uses keeps the sidecar hot.
		s, err := r.Evaluate(ctx, batch.Traces[0])
		if err != nil {
			return [][]Score{nil}, nil
		}
		return [][]Score{s}, nil
	}
	items := make([]RemoteEvaluatorRequest, 0, len(batch.Traces))
	for _, t := range batch.Traces {
		if t.InputMessages == "" || t.OutputMessages == "" {
			items = append(items, RemoteEvaluatorRequest{TraceID: t.TraceID, Model: t.RequestModel})
			continue
		}
		items = append(items, r.buildRequest(t, ov))
	}
	body, _ := json.Marshal(remoteBatchRequest{Items: items})
	raw, err := r.post(ctx, "/evaluate/batch", body)
	if err != nil {
		return nil, err
	}
	var rb remoteBatchResponse
	if err := json.Unmarshal(raw, &rb); err != nil {
		return nil, err
	}
	out := make([][]Score, len(batch.Traces))
	for i, it := range items {
		scs := rb.ScoresByTrace[it.TraceID]
		out[i] = make([]Score, 0, len(scs))
		for _, s := range scs {
			evaluator := s.Evaluator
			if evaluator == "" {
				evaluator = "remote_eval"
			}
			out[i] = append(out[i], Score{
				TraceID:    it.TraceID,
				Timestamp:  time.Now().UTC(),
				Evaluator:  evaluator,
				Metric:     s.Metric,
				Score:      clamp01(s.Score),
				Passed:     s.Passed,
				Rationale:  s.Rationale,
				JudgeModel: s.JudgeModel,
			})
		}
	}
	return out, nil
}

// Evaluate processes a single trace (the existing behaviour); used by
// non-batched callers and as the singleton fast-path of EvaluateBatch.
func (r *RemoteEvaluator) Evaluate(ctx context.Context, t observability.Trace) ([]Score, error) {
	if t.InputMessages == "" || t.OutputMessages == "" {
		return nil, nil
	}
	body, _ := json.Marshal(r.buildRequest(t, nil))
	raw, err := r.post(ctx, "/evaluate", body)
	if err != nil {
		return nil, err
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
