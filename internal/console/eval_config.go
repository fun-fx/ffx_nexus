package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/core"
)

// EvalConfigSnapshot is the effective eval + routing configuration exposed to
// the console. Secrets are never returned in plaintext.
type EvalConfigSnapshot struct {
	EvalEnabled       bool   `json:"eval_enabled"`
	RoutingEnabled    bool   `json:"routing_enabled"`
	ScoreStore        string `json:"score_store"`         // clickhouse | postgres | noop
	TraceStore        string `json:"trace_store"`         // clickhouse | live_only
	ScorePersisted    bool   `json:"score_persisted"`     // true when scores land in durable storage
	RoutingStatsStore string `json:"routing_stats_store"` // clickhouse | postgres | empty
	Eval              struct {
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

		// BenchBlend is the operational surface for PrimeIntellect
		// (or any external benchmark platform) wired into routing.
		// Weight and HalfLife are READ-ONLY env vars on this build —
		// rotating them while the router is live would invalidate
		// every cached Stats value, so the patch surface intentionally
		// rejects writes. Operators wanting to change them restart the
		// pod after editing NEXUS_ROUTE_W_BENCH /
		// NEXUS_ROUTE_BENCH_HALF_LIFE. The console surfaces this in
		// RestartRequired below.
		//
		// BenchEnabled is a derived field so the UI can render "bench
		// blend is wired" vs. "not configured" without reading envs.
		BenchEnabled bool    `json:"bench_enabled"`
		BenchWeight  float64 `json:"bench_weight"`
		BenchDecay   string  `json:"bench_decay"`
	} `json:"routing"`
	// PluginOnly is true when NEXUS_EVAL_PLUGIN_ONLY is set at
	// boot — the runtime controller did not seed built-in
	// heuristic profiles on this pod. Operators use this to
	// surface "this cluster only scores via plugins" in the
	// console without gutting the existing store contents.
	PluginOnly bool `json:"plugin_only"`
	// PurgeLegacyProfilesOnBoot is the *destructive* companion
	// flag. Paired with PluginOnly it tells the controller to
	// hard-delete the four well-known default rows
	// (default-pii, default-completeness, default-judge,
	// default-remote) on each boot. The console surfaces a
	// persistent banner whenever this is on so admins can
	// correlate after-the-fact deletions with their explicit
	// config change.
	PurgeLegacyProfilesOnBoot bool     `json:"purge_legacy_profiles_on_boot"`
	RestartRequired           []string `json:"restart_required"`
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

	// Nested form accepted from the new console. Some callers (the legacy
	// admin scripts and older console builds) still POST the flat top-level
	// fields above. Apply() prefers the nested form when present, then
	// falls back to the flat fields, so both shapes update the same cells.
	Eval *EvalConfigPatchEval `json:"eval"`
}

// EvalConfigPatchEval mirrors the nested shape the console sends on
// /api/eval/config PATCH. PIIEnabled/CompletenessEnabled are the only
// cells that benefit from the nested form today — sample rate, workers,
// and judge/remote are still env-driven.
type EvalConfigPatchEval struct {
	PIIEnabled          *bool    `json:"pii_enabled"`
	CompletenessEnabled *bool    `json:"completeness_enabled"`
	SampleRate          *float64 `json:"sample_rate"`
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
		s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.config.update", "", config.FormatRouteGroups(snap.Routing.Groups))
	writeJSON(w, http.StatusOK, snap)
}
