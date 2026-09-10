package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/gateway"
	"github.com/ffxnexus/nexus/internal/guardrails"
	"github.com/ffxnexus/nexus/internal/router"
	"github.com/ffxnexus/nexus/internal/semcache"
)

type gatewayRuntimeController struct {
	mu sync.Mutex

	cfg             config.Config
	gwHandler       *gateway.Handler
	semCacheService *semcache.Service
	semCacheBooted  bool
	log             *slog.Logger
}

func newGatewayRuntimeController(
	cfg config.Config,
	gwHandler *gateway.Handler,
	semCacheService *semcache.Service,
	log *slog.Logger,
) *gatewayRuntimeController {
	return &gatewayRuntimeController{
		cfg:             cfg,
		gwHandler:       gwHandler,
		semCacheService: semCacheService,
		semCacheBooted:  semCacheService != nil,
		log:             log,
	}
}

func (c *gatewayRuntimeController) Snapshot() console.GatewayConfigSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	var snap console.GatewayConfigSnapshot
	snap.Guardrails.Enabled = c.cfg.GuardrailsEnabled
	snap.Guardrails.BlockPIIInput = c.cfg.GuardrailBlockPIIIn
	snap.Guardrails.RedactPIIOutput = c.cfg.GuardrailRedactPIIOut
	snap.Guardrails.MaxInputChars = c.cfg.GuardrailMaxInputChrs
	snap.Guardrails.DenyPatterns = splitDenyPatterns(c.cfg.GuardrailDenyPatterns)
	snap.Guardrails.ValidateJSONOutput = c.cfg.GuardrailValidateJSON
	snap.Guardrails.SelfCorrectionEnabled = c.cfg.SelfCorrectionEnabled
	snap.Guardrails.SelfCorrectionMaxRetries = c.cfg.SelfCorrectionMaxRetries

	snap.SemanticCache.Enabled = c.cfg.SemanticCacheEnabled
	snap.SemanticCache.TTL = formatDuration(c.cfg.SemanticCacheTTL)
	snap.SemanticCache.Threshold = c.cfg.SemanticCacheThreshold
	snap.SemanticCache.MaxEntries = c.cfg.SemanticCacheMaxEntries
	snap.SemanticCache.RedisConfigured = c.cfg.RedisURL != ""
	snap.SemanticCache.EmbeddingsConfigured = c.cfg.EmbeddingsURL != ""

	snap.Alerting.FailoverWebhookSet = strings.TrimSpace(c.cfg.FailoverWebhookURL) != ""
	snap.Alerting.FailoverSlackSet = strings.TrimSpace(c.cfg.FailoverSlackURL) != ""
	snap.Alerting.Cooldown = formatDuration(c.cfg.FailoverAlertCooldown)
	return snap
}

func (c *gatewayRuntimeController) Apply(patch console.GatewayConfigPatch) (console.GatewayConfigSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if g := patch.Guardrails; g != nil {
		if g.Enabled != nil {
			c.cfg.GuardrailsEnabled = *g.Enabled
		}
		if g.BlockPIIInput != nil {
			c.cfg.GuardrailBlockPIIIn = *g.BlockPIIInput
		}
		if g.RedactPIIOutput != nil {
			c.cfg.GuardrailRedactPIIOut = *g.RedactPIIOutput
		}
		if g.MaxInputChars != nil {
			c.cfg.GuardrailMaxInputChrs = *g.MaxInputChars
		}
		if g.DenyPatterns != nil {
			c.cfg.GuardrailDenyPatterns = strings.Join(*g.DenyPatterns, ";")
		}
		if g.ValidateJSONOutput != nil {
			c.cfg.GuardrailValidateJSON = *g.ValidateJSONOutput
		}
		if g.SelfCorrectionEnabled != nil {
			c.cfg.SelfCorrectionEnabled = *g.SelfCorrectionEnabled
		}
		if g.SelfCorrectionMaxRetries != nil {
			c.cfg.SelfCorrectionMaxRetries = *g.SelfCorrectionMaxRetries
		}
		c.applyGuardrailsLocked()
	}

	if sc := patch.SemanticCache; sc != nil {
		if sc.Enabled != nil {
			if *sc.Enabled && !c.semCacheBooted {
				return console.GatewayConfigSnapshot{}, fmt.Errorf("semantic cache requires Redis and embeddings at boot; set NEXUS_SEMANTIC_CACHE_ENABLED and restart")
			}
			c.cfg.SemanticCacheEnabled = *sc.Enabled
			if c.gwHandler != nil {
				if *sc.Enabled && c.semCacheService != nil {
					c.gwHandler.SetSemanticCache(c.semCacheService)
				} else {
					c.gwHandler.SetSemanticCache(nil)
				}
			}
		}
		if sc.TTL != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*sc.TTL))
			if err != nil {
				return console.GatewayConfigSnapshot{}, err
			}
			c.cfg.SemanticCacheTTL = d
		}
		if sc.Threshold != nil {
			c.cfg.SemanticCacheThreshold = *sc.Threshold
		}
		if sc.MaxEntries != nil {
			c.cfg.SemanticCacheMaxEntries = *sc.MaxEntries
		}
		if c.semCacheService != nil && c.cfg.SemanticCacheEnabled {
			c.semCacheService.UpdateConfig(semcache.Config{
				Enabled:            true,
				TTL:                c.cfg.SemanticCacheTTL,
				Threshold:          c.cfg.SemanticCacheThreshold,
				MaxEntriesPerModel: c.cfg.SemanticCacheMaxEntries,
			})
		}
	}

	if a := patch.Alerting; a != nil {
		if a.FailoverWebhook != nil {
			c.cfg.FailoverWebhookURL = strings.TrimSpace(*a.FailoverWebhook)
		}
		if a.FailoverSlack != nil {
			c.cfg.FailoverSlackURL = strings.TrimSpace(*a.FailoverSlack)
		}
		if a.Cooldown != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*a.Cooldown))
			if err != nil {
				return console.GatewayConfigSnapshot{}, err
			}
			c.cfg.FailoverAlertCooldown = d
		}
		c.applyFailoverLocked()
	}

	return c.snapshotLocked(), nil
}

func (c *gatewayRuntimeController) applyGuardrailsLocked() {
	if c.gwHandler == nil {
		return
	}
	guard := guardrails.New(guardrails.Config{
		Enabled:            c.cfg.GuardrailsEnabled,
		BlockPIIInput:      c.cfg.GuardrailBlockPIIIn,
		RedactPIIOutput:    c.cfg.GuardrailRedactPIIOut,
		MaxInputChars:      c.cfg.GuardrailMaxInputChrs,
		DenyPatterns:       splitDenyPatterns(c.cfg.GuardrailDenyPatterns),
		ValidateJSONOutput: c.cfg.GuardrailValidateJSON,
	})
	if guard != nil && guard.Active() {
		c.gwHandler.SetGuard(guard)
	} else {
		c.gwHandler.SetGuard(nil)
	}
	retries := 0
	if c.cfg.SelfCorrectionEnabled {
		retries = c.cfg.SelfCorrectionMaxRetries
	}
	c.gwHandler.SetSelfCorrection(retries)
}

func (c *gatewayRuntimeController) applyFailoverLocked() {
	if c.gwHandler == nil {
		return
	}
	var sinks []router.Notifier
	if c.cfg.FailoverWebhookURL != "" {
		sinks = append(sinks, router.NewWebhookNotifier(c.cfg.FailoverWebhookURL, c.log))
	}
	if c.cfg.FailoverSlackURL != "" {
		sinks = append(sinks, router.NewSlackNotifier(c.cfg.FailoverSlackURL, c.log))
	}
	if mn := router.NewMultiNotifier(sinks...); mn != nil {
		c.gwHandler.SetFailoverNotifier(mn)
	} else {
		c.gwHandler.SetFailoverNotifier(nil)
	}
}

func (c *gatewayRuntimeController) snapshotLocked() console.GatewayConfigSnapshot {
	var snap console.GatewayConfigSnapshot
	snap.Guardrails.Enabled = c.cfg.GuardrailsEnabled
	snap.Guardrails.BlockPIIInput = c.cfg.GuardrailBlockPIIIn
	snap.Guardrails.RedactPIIOutput = c.cfg.GuardrailRedactPIIOut
	snap.Guardrails.MaxInputChars = c.cfg.GuardrailMaxInputChrs
	snap.Guardrails.DenyPatterns = splitDenyPatterns(c.cfg.GuardrailDenyPatterns)
	snap.Guardrails.ValidateJSONOutput = c.cfg.GuardrailValidateJSON
	snap.Guardrails.SelfCorrectionEnabled = c.cfg.SelfCorrectionEnabled
	snap.Guardrails.SelfCorrectionMaxRetries = c.cfg.SelfCorrectionMaxRetries
	snap.SemanticCache.Enabled = c.cfg.SemanticCacheEnabled
	snap.SemanticCache.TTL = formatDuration(c.cfg.SemanticCacheTTL)
	snap.SemanticCache.Threshold = c.cfg.SemanticCacheThreshold
	snap.SemanticCache.MaxEntries = c.cfg.SemanticCacheMaxEntries
	snap.SemanticCache.RedisConfigured = c.cfg.RedisURL != ""
	snap.SemanticCache.EmbeddingsConfigured = c.cfg.EmbeddingsURL != ""
	snap.Alerting.FailoverWebhookSet = strings.TrimSpace(c.cfg.FailoverWebhookURL) != ""
	snap.Alerting.FailoverSlackSet = strings.TrimSpace(c.cfg.FailoverSlackURL) != ""
	snap.Alerting.Cooldown = formatDuration(c.cfg.FailoverAlertCooldown)
	return snap
}
