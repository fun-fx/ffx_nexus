package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/core"
)

// EvalConfigSnapshot is the effective eval + routing configuration exposed to
// the console. Secrets are never returned in plaintext.
type EvalConfigSnapshot struct {
	EvalEnabled    bool   `json:"eval_enabled"`
	RoutingEnabled bool   `json:"routing_enabled"`
	ScoreStore     string `json:"score_store"`     // clickhouse | noop
	TraceStore     string `json:"trace_store"`     // clickhouse | live_only
	ScorePersisted bool   `json:"score_persisted"` // true when scores land in durable storage
	Eval           struct {
		PIIEnabled          bool    `json:"pii_enabled"`
		CompletenessEnabled bool    `json:"completeness_enabled"`
		SampleRate          float64 `json:"sample_rate"`
		Workers             int     `json:"workers"`
		Judge               struct {
			Enabled   bool   `json:"enabled"`
			BaseURL   string `json:"base_url"`
			Model     string `json:"model"`
			APIKeySet bool   `json:"api_key_set"`
		} `json:"judge"`
		Remote struct {
			Enabled bool     `json:"enabled"`
			URL     string   `json:"url"`
			Metrics []string `json:"metrics"`
			Timeout string   `json:"timeout"`
		} `json:"remote"`
	} `json:"eval"`
	Routing struct {
		Weights     map[string]float64  `json:"weights"`
		Window      string              `json:"window"`
		Refresh     string              `json:"refresh"`
		Groups      map[string][]string `json:"groups"`
		GroupsSpec  string              `json:"groups_spec"`
		LoadBalance bool                `json:"load_balance"`
	} `json:"routing"`
	RestartRequired []string `json:"restart_required"`
}

type EvalConfigPatch struct {
	PIIEnabled          *bool    `json:"pii_enabled"`
	CompletenessEnabled *bool    `json:"completeness_enabled"`
	SampleRate          *float64 `json:"sample_rate"`
	JudgeBaseURL        *string  `json:"judge_base_url"`
	JudgeModel          *string  `json:"judge_model"`
	JudgeAPIKey         *string  `json:"judge_api_key"`
	EvalServiceURL      *string  `json:"eval_service_url"`
	EvalServiceMetrics  *string  `json:"eval_service_metrics"`
	RouteWQuality       *float64 `json:"route_w_quality"`
	RouteWCost          *float64 `json:"route_w_cost"`
	RouteWLatency       *float64 `json:"route_w_latency"`
	RouteWindow         *string  `json:"route_window"`
	RouteGroups         *string  `json:"route_groups"`
}

// EvalConfigSource supplies the current effective eval/routing snapshot.
type EvalConfigSource interface {
	Snapshot() EvalConfigSnapshot
}

// EvalConfigApplier applies runtime changes from the console (admin PATCH).
type EvalConfigApplier interface {
	Apply(patch EvalConfigPatch) (EvalConfigSnapshot, error)
}

func (s *Server) getEvalConfig(w http.ResponseWriter, _ *http.Request, _ core.User) {
	if s.evalConfigSrc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "eval config unavailable (eval worker disabled)",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.evalConfigSrc.Snapshot())
}

func (s *Server) patchEvalConfig(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalConfigApply == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "eval config unavailable (eval worker disabled)",
		})
		return
	}
	var patch EvalConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if patch.SampleRate != nil && (*patch.SampleRate < 0 || *patch.SampleRate > 1) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sample_rate must be between 0 and 1"})
		return
	}
	if patch.RouteWindow != nil {
		if _, err := time.ParseDuration(strings.TrimSpace(*patch.RouteWindow)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "route_window must be a duration like 1h"})
			return
		}
	}
	snap, err := s.evalConfigApply.Apply(patch)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.config.update", "", config.FormatRouteGroups(snap.Routing.Groups))
	writeJSON(w, http.StatusOK, snap)
}
