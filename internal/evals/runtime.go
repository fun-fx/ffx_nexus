package evals

import (
	"strings"
	"time"
)

// RuntimeState is a point-in-time view of worker eval settings.
type RuntimeState struct {
	PIIEnabled          bool
	CompletenessEnabled bool
	SampleRate          float64
	Workers             int
	JudgeBaseURL        string
	JudgeModel          string
	JudgeAPIKeySet      bool
	RemoteURL           string
	RemoteMetrics       []string
	RemoteTimeout       time.Duration
	JudgeEnabled        bool
	RemoteEnabled       bool
}

// RuntimeState returns the current worker eval configuration.
func (w *Worker) RuntimeState() RuntimeState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return RuntimeState{
		PIIEnabled:          w.piiEnabled,
		CompletenessEnabled: w.completenessEnabled,
		SampleRate:          w.judgeSampleRate,
		Workers:             w.workerCount,
		JudgeBaseURL:        w.judgeBaseURL,
		JudgeModel:          w.judgeModel,
		JudgeAPIKeySet:      w.judgeAPIKey != "",
		RemoteURL:           w.remoteURL,
		RemoteMetrics:       append([]string(nil), w.remoteMetrics...),
		RemoteTimeout:       w.remoteTimeout,
		JudgeEnabled:        w.judgeBaseURL != "" && w.judgeModel != "",
		RemoteEnabled:       w.remoteURL != "",
	}
}

// SetPIIEnabled toggles the heuristic PII evaluator at runtime.
func (w *Worker) SetPIIEnabled(on bool) {
	w.mu.Lock()
	w.piiEnabled = on
	w.mu.Unlock()
}

// SetCompletenessEnabled toggles the heuristic completeness evaluator.
func (w *Worker) SetCompletenessEnabled(on bool) {
	w.mu.Lock()
	w.completenessEnabled = on
	w.mu.Unlock()
}

// SetJudgeSampleRate sets the fraction of traces sent to expensive judges.
func (w *Worker) SetJudgeSampleRate(rate float64) {
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	w.mu.Lock()
	w.judgeSampleRate = rate
	w.mu.Unlock()
}

// JudgeConfig holds SLM judge connection settings.
type JudgeRuntimeConfig struct {
	BaseURL string
	Model   string
	APIKey  string // empty keeps existing key when updating other fields
}

// RemoteRuntimeConfig holds external eval service settings.
type RemoteRuntimeConfig struct {
	URL     string
	Metrics []string
	Timeout time.Duration
}

// ConfigureJudges rebuilds expensive evaluators from runtime settings.
func (w *Worker) ConfigureJudges(judge JudgeRuntimeConfig, remote RemoteRuntimeConfig) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.judgeBaseURL = strings.TrimSpace(judge.BaseURL)
	w.judgeModel = strings.TrimSpace(judge.Model)
	if judge.APIKey != "" {
		w.judgeAPIKey = judge.APIKey
	}
	w.remoteURL = strings.TrimSpace(remote.URL)
	if remote.Metrics != nil {
		w.remoteMetrics = append([]string(nil), remote.Metrics...)
	}
	if remote.Timeout > 0 {
		w.remoteTimeout = remote.Timeout
	}

	var judges []Evaluator
	if j := NewSLMJudge(JudgeConfig{
		BaseURL: w.judgeBaseURL,
		Model:   w.judgeModel,
		APIKey:  w.judgeAPIKey,
	}); j != nil {
		judges = append(judges, j)
	}
	if r := NewRemoteEvaluator(RemoteConfig{
		BaseURL: w.remoteURL,
		Metrics: w.remoteMetrics,
		Timeout: w.remoteTimeout,
	}); r != nil {
		judges = append(judges, r)
	}
	w.judges = judges
}
