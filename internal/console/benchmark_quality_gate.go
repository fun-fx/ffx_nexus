package console

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/router"
)

// benchmarkQualityGate is the CI gate endpoint a release pipeline
// hits before promoting a build. Body shape:
//
//	{
//	  "models": ["openai/gpt-4o-mini"],
//	  "min_avg_score": 0.7,
//	  "max_stale_days": 14,
//	  "dry_run": true
//	}
//
// A request without `models` checks every model on the operator's
// lineup; `min_avg_score` and `max_stale_days` are optional and
// default to a permissive baseline. dry_run=true means "report
// pass/fail without writing audit_log rows", so a smoke test can be
// safely run in CI without polluting the audit log.
//
// The handler returns 200 with `{ok: true, results: [...]}`
// regardless of pass/fail so a CI runner can write a one-liner on
// failures. Sticking to 200 is the same convention as the eval
// plugins' test endpoint — Cloudflare and similar CDNs can replace
// 4xx with branded HTML, and a typed JSON gate body survives.
// Real failure cases (missing router, bad JSON) still use 4xx
// because nothing would be visible to fix without them.
type benchmarkQualityGateRequest struct {
	Models       []string `json:"models"`
	MinAvgScore  float64  `json:"min_avg_score"`
	MaxStaleDays float64  `json:"max_stale_days"`
	DryRun       bool     `json:"dry_run"`
}

type benchmarkQualityGateResult struct {
	Model       string    `json:"model"`
	AvgScore    float64   `json:"avg_score"`
	CompletedAt time.Time `json:"completed_at"`
	StaleDays   float64   `json:"stale_days"`
	Freshness   float64   `json:"freshness"`
	Pass        bool      `json:"pass"`
	Reasons     []string  `json:"reasons,omitempty"`
}

type benchmarkQualityGateResponse struct {
	OK      bool                         `json:"ok"`
	Summary benchmarkQualityGateSummary  `json:"summary"`
	Results []benchmarkQualityGateResult `json:"results"`
	AtTime  time.Time                    `json:"at"`
	Weights router.CombinedWeights       `json:"weights"`
}

type benchmarkQualityGateSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

func (s *Server) benchmarkQualityGate(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.benchmarks == nil || s.qualityRouter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "benchmarks / quality router not wired",
		})
		return
	}
	var req benchmarkQualityGateRequest
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid JSON",
			})
			return
		}
	}
	if req.MaxStaleDays == 0 {
		req.MaxStaleDays = 30
	}

	weights, halfLife, src := s.qualityRouter.BlendConfig()
	snap, err := src.BenchmarkSnapshot(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "benchmark snapshot failed: " + err.Error(),
		})
		return
	}

	targets := req.Models
	if len(targets) == 0 {
		targets = s.qualityRouter.KnownModels(r.Context())
	}

	now := time.Now().UTC()
	resp := benchmarkQualityGateResponse{
		AtTime:  now,
		Weights: weights,
	}
	for _, m := range targets {
		stat, ok := snap[m]
		result := benchmarkQualityGateResult{Model: m, Pass: true}
		if !ok {
			result.Pass = false
			result.Reasons = []string{"no_settled_benchmark"}
			resp.Summary.Total++
			resp.Summary.Failed++
			resp.Results = append(resp.Results, result)
			continue
		}
		result.AvgScore = stat.AvgScore
		result.CompletedAt = stat.CompletedAt
		result.StaleDays = now.Sub(stat.CompletedAt).Hours() / 24
		result.Freshness = router.Freshness(stat, halfLife, now)

		if req.MinAvgScore > 0 && stat.AvgScore < req.MinAvgScore {
			result.Reasons = append(result.Reasons, "below_min_avg_score")
		}
		if result.StaleDays > req.MaxStaleDays {
			result.Reasons = append(result.Reasons, "stale_beyond_max")
		}
		if result.Freshness <= 0 {
			result.Reasons = append(result.Reasons, "freshness_zero")
		}
		result.Pass = len(result.Reasons) == 0
		resp.Summary.Total++
		if result.Pass {
			resp.Summary.Passed++
		} else {
			resp.Summary.Failed++
		}
		resp.Results = append(resp.Results, result)
	}
	resp.OK = resp.Summary.Failed == 0

	if !req.DryRun && !resp.OK && s.benchmarks != nil {
		// Audit the gate failure with a non-dry-run call so a
		// failing build leaves a permanent record. Pass/gate-fail
		// audits are intentionally not written; the audit log is
		// for events, not "the world is normal".
		if auditor, ok := s.benchmarks.(interface {
			AuditGate(ctx context.Context, payload string)
		}); ok {
			payload, _ := json.Marshal(auditPayload{u.Email, resp})
			auditor.AuditGate(r.Context(), string(payload))
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type auditPayload struct {
	Email string                       `json:"actor"`
	Resp  benchmarkQualityGateResponse `json:"resp"`
}

// (No further code. PR-5 is the gate endpoint itself; the optional
// audit hook is documented but left optional — implementations may
// add it to BenchmarkRunner at a later PR.)
