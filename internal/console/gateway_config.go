package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/core"
)

// GatewayConfigSnapshot exposes hot-path gateway policy knobs to the console.
type GatewayConfigSnapshot struct {
	Guardrails struct {
		Enabled            bool     `json:"enabled"`
		BlockPIIInput        bool     `json:"block_pii_input"`
		RedactPIIOutput      bool     `json:"redact_pii_output"`
		MaxInputChars        int      `json:"max_input_chars"`
		DenyPatterns         []string `json:"deny_patterns"`
		ValidateJSONOutput   bool     `json:"validate_json_output"`
		SelfCorrectionEnabled bool    `json:"self_correction_enabled"`
		SelfCorrectionMaxRetries int  `json:"self_correction_max_retries"`
	} `json:"guardrails"`
	SemanticCache struct {
		Enabled        bool    `json:"enabled"`
		TTL            string  `json:"ttl"`
		Threshold      float64 `json:"threshold"`
		MaxEntries     int     `json:"max_entries"`
		RedisConfigured bool   `json:"redis_configured"`
		EmbeddingsConfigured bool `json:"embeddings_configured"`
	} `json:"semantic_cache"`
	Alerting struct {
		FailoverWebhookSet bool   `json:"failover_webhook_set"`
		FailoverSlackSet   bool   `json:"failover_slack_set"`
		Cooldown           string `json:"cooldown"`
	} `json:"alerting"`
	RestartRequired []string `json:"restart_required"`
}

type GatewayConfigPatch struct {
	Guardrails *struct {
		Enabled              *bool    `json:"enabled"`
		BlockPIIInput        *bool    `json:"block_pii_input"`
		RedactPIIOutput      *bool    `json:"redact_pii_output"`
		MaxInputChars        *int     `json:"max_input_chars"`
		DenyPatterns         *[]string `json:"deny_patterns"`
		ValidateJSONOutput   *bool    `json:"validate_json_output"`
		SelfCorrectionEnabled *bool   `json:"self_correction_enabled"`
		SelfCorrectionMaxRetries *int `json:"self_correction_max_retries"`
	} `json:"guardrails"`
	SemanticCache *struct {
		Enabled    *bool    `json:"enabled"`
		TTL        *string  `json:"ttl"`
		Threshold  *float64 `json:"threshold"`
		MaxEntries *int     `json:"max_entries"`
	} `json:"semantic_cache"`
	Alerting *struct {
		FailoverWebhook *string `json:"failover_webhook"`
		FailoverSlack   *string `json:"failover_slack"`
		Cooldown        *string `json:"cooldown"`
	} `json:"alerting"`
}

type GatewayConfigSource interface {
	Snapshot() GatewayConfigSnapshot
}

type GatewayConfigApplier interface {
	Apply(patch GatewayConfigPatch) (GatewayConfigSnapshot, error)
}

func (s *Server) getGatewayConfig(w http.ResponseWriter, _ *http.Request, _ core.User) {
	if s.gatewayConfigSrc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "gateway config unavailable",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.gatewayConfigSrc.Snapshot())
}

func (s *Server) patchGatewayConfig(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.gatewayConfigApply == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "gateway config unavailable",
		})
		return
	}
	var patch GatewayConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if patch.SemanticCache != nil && patch.SemanticCache.Threshold != nil {
		t := *patch.SemanticCache.Threshold
		if t < 0 || t > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "threshold must be between 0 and 1"})
			return
		}
	}
	if patch.SemanticCache != nil && patch.SemanticCache.TTL != nil {
		if _, err := time.ParseDuration(strings.TrimSpace(*patch.SemanticCache.TTL)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ttl must be a duration like 24h"})
			return
		}
	}
	snap, err := s.gatewayConfigApply.Apply(patch)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("gateway.config.update"), "", "")
	writeJSON(w, http.StatusOK, snap)
}
