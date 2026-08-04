// langsmith_rules.go wires the vendor-side automator behind the
// admin REST route /api/eval/plugins/{name}/automation.
//
// Why a separate file:
//
//   - langsmith.go is the trace-ingest + heartbeat adapter (read +
//     write the data plane).
//   - The automator is a control-plane integration (write the
//     vendor's *configuration* through its public REST API).
//
// Folding the two would imply they share state; they do not. The
// automator needs only an API key and a session id, while trace
// ingestion needs the same key plus a streaming transport. Keeping
// them split also means a future LangSmith API version bump can
// add a router with both versions side-by-side.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/langsmith/rules"
	"github.com/ffxnexus/nexus/internal/observability"
)

// langsmithRuleAutomator is the production implementation of
// console.LangSmithRuleCreator. It is constructed in main() once
// and wired via SetLangSmithRuleCreator. The wiring lives there
// (not here) because console/compile-time references are not
// allowed inside the cmd package tree's runtime graph.
type langsmithRuleAutomator struct {
	cfg     LangsmithAutomatorConfig
	log     *slog.Logger
	plugins console.EvalPluginSource
	keys    secretsResolver
}

// LangsmithAutomatorConfig groups the dependencies the automator
// needs from cmd/nexus/main.go. Kept in a struct so a test can
// stub each field without exposing the automator's internals.
type LangsmithAutomatorConfig struct {
	// BaseURL is the operator-facing Nexus base (the one
	// LangSmith must POST to). When empty the automator refuses
	// to act because Nexus cannot derive an externally-routable
	// callback URL on its own — the console server is typically
	// behind an ingress proxy that rewrites /api/eval/*.
	BaseURL string
	// EndpointOverride is the langsmith.endpoint per-plugin
	// override (defaults to api.smith.langchain.com). Usually
	// empty; populated when the operator runs a self-hosted
	// LangSmith tenant and declared it on the EvalPlugin spec.
	EndpointOverride string
	// HeaderName is set on every webhook this automator creates,
	// so Nexus's collector can identify the rule that called it
	// during audit. The value is the plugin name; the header is
	// non-secret and surfaced in the rule's "active webhooks"
	// list.
	HeaderName string
	// Timeout caps the outbound automator call independently of
	// the request context — Cloudflare's 60s gate is the floor
	// we want to come in under; 30s leaves headroom. The auth
	// timeout in the LangSmith path of plugin_tester.go is 5s
	// for the cheaper /info probe; here we use a longer budget
	// because creating a rule is more work than reading one.
	Timeout time.Duration
}

// secretsResolver is the small surface the automator needs from
// the plugin-key vault. We don't import the full Resolver to
// keep the package boundary thin — both the admin REST handler
// and the automator operate on the same (name → key map).
type secretsResolver interface {
	ResolveSecret(pluginName, keyRef string) (string, error)
}

// keyVaultSecretsResolver adapts the in-process pluginSecrets
// hydrator (the single active surface per cmd/nexus/plugin_keys.go)
// to the secretsResolver interface the automator consumes. We do
// the adaptation here (rather than reusing the broader
// evalplugin.AuthSpec resolver) because the automator knows the
// keyRef format up front: LangSmith uses a single key name, not
// a public/secret pair.
type keyVaultSecretsResolver struct {
	get func(plugin string) (map[string]string, bool)
}

// ResolveSecret returns the named key from the plugin's vault.
// The boolean Get returns is dropped — we treat a missing key as
// an error rather than an empty string, so a typo in keyRef
// surfaces in the admin REST response rather than as a 401 from
// LangSmith (which would look like the key was rejected).
func (k keyVaultSecretsResolver) ResolveSecret(plugin, keyRef string) (string, error) {
	if k.get == nil {
		return "", errors.New("plugin-secrets hydrator not wired")
	}
	keys, ok := k.get(plugin)
	if !ok {
		return "", fmt.Errorf("plugin %q has no stored keys", plugin)
	}
	v, ok := keys[keyRef]
	if !ok {
		return "", fmt.Errorf("plugin %q is missing key %q", plugin, keyRef)
	}
	if v == "" {
		return "", fmt.Errorf("plugin %q key %q is empty", plugin, keyRef)
	}
	return v, nil
}

// CreateAutomationRule resolves the plugin's LangSmith
// credentials, computes the callback URL, and posts the rule to
// /api/v1/runs/rules. The typed result map lets the admin REST
// handler return a 200 OK status on every flow path — including
// ErrConflict (already configured) — so the React UI never has
// to fall back to "fetch failed" displays.
//
// SessionID is the LangSmith project UUID. We accept it from
// the request body because the operator's LangSmith tenant
// identifies projects with opaque UUIDs that Nexus has no way
// to enumerate without its own API token.
func (a *langsmithRuleAutomator) CreateAutomationRule(ctx context.Context, pluginName, sessionID string) (console.AutomationRuleResult, error) {
	if strings.TrimSpace(a.cfg.BaseURL) == "" {
		return console.AutomationRuleResult{}, errors.New("NEXUS_PUBLIC_BASE_URL is not set; Nexus cannot derive the webhook callback URL")
	}
	if strings.TrimSpace(sessionID) == "" {
		return console.AutomationRuleResult{}, errors.New("session_id is required (paste the LangSmith project UUID from Settings → Projects)")
	}
	pluginRec, err := a.plugins.Lookup(ctx, pluginName)
	if err != nil {
		// ErrPluginNotFound is mapped to a typed message upstream;
		// pass through.
		return console.AutomationRuleResult{}, fmt.Errorf("plugin %q not found: %w", pluginName, err)
	}
	pluginSpec, err := evalplugin.Decode([]byte(pluginRec.SpecYAML))
	if err != nil {
		return console.AutomationRuleResult{}, fmt.Errorf("plugin %q spec invalid: %w", pluginName, err)
	}
	auth := pluginSpec.Spec.Service.Auth
	keyRef := auth.KeyRef
	if keyRef == "" {
		return console.AutomationRuleResult{}, errors.New("plugin manifest does not declare auth.keyRef; cannot resolve LangSmith API key")
	}
	// LangSmith uses a single API key (not a public/secret pair),
	// so keyRef is a single name. The ResolveSecret path treats
	// the resolved list as a single string in this case.
	plaintext, err := a.keys.ResolveSecret(pluginName, keyRef)
	if err != nil {
		return console.AutomationRuleResult{}, fmt.Errorf("could not resolve LangSmith API key from vault: %w", err)
	}

	webhookURL := computeWebhookURL(a.cfg.BaseURL, pluginName)
	endpoint := a.cfg.EndpointOverride
	if endpoint == "" {
		endpoint = pluginSpec.Spec.Service.Endpoint
	}

	clientCtx, cancel := context.WithTimeout(ctx, a.timeoutOrDefault())
	defer cancel()
	c := rules.New(endpoint, plaintext)
	rule, err := c.CreateRule(clientCtx, rules.CreateRuleRequest{
		DisplayName:  fmt.Sprintf("Nexus %s webhook", pluginName),
		SessionID:    sessionID,
		IsEnabled:    true,
		SamplingRate: 1.0,
		Webhooks: []rules.Webhook{{
			URL: webhookURL,
			Headers: map[string]string{
				a.headerOrDefault(): pluginName,
			},
		}},
	}, webhookURL)
	if err != nil {
		// Map the typed sentinels to the typed AutomationRuleResult
		// fields, so the admin REST handler can branch on
		// AlreadyConfigured without parsing strings.
		var res console.AutomationRuleResult
		res.WebhookURL = webhookURL
		switch {
		case errors.Is(err, rules.ErrConflict):
			res.AlreadyConfigured = true
			res.Message = "LangSmith already has a rule with this display name in this project. Open the project and verify the webhook, or rename the plugin."
			return res, err
		case errors.Is(err, rules.ErrUnauthorized):
			res.Message = "LangSmith rejected the API key. Verify the key in the Plugin Keys panel and that the key belongs to the same workspace as the project."
			return res, err
		case errors.Is(err, rules.ErrInvalidRequest):
			res.Message = "LangSmith rejected the request body. The session_id should be a project UUID from Settings → Projects."
			return res, err
		case errors.Is(err, rules.ErrUpstream):
			res.Message = "LangSmith returned a non-success response. Try again in a minute; Cloudflare occasionally 5xx's /api/v1/*."
			return res, err
		case strings.Contains(err.Error(), "validation"):
			res.Message = err.Error()
			return res, err
		default:
			res.Message = err.Error()
			return res, err
		}
	}
	a.log.Info("langsmith automation rule created",
		"plugin", pluginName,
		"rule_id", rule.ID,
		"session_id", sessionID,
		"webhook_url", webhookURL,
	)
	return console.AutomationRuleResult{
		OK:         true,
		RuleID:     rule.ID,
		WebhookURL: webhookURL,
		Message:    fmt.Sprintf("LangSmith automation rule created for plugin %q", pluginName),
	}, nil
}

// computeWebhookURL joins the operator-supplied base URL with
// the per-plugin webhook path. We use url.JoinPath (not string
// concat) so a base with a trailing slash and a path with a
// leading slash cannot produce a malformed URL like "https://n/"
// + "/api/..." which would 404 every call.
func computeWebhookURL(base, pluginName string) string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		// Defensive: a malformed base URL we'll fail elsewhere
		// (CreateAutomationRule checks the env at the entry).
		// Here we just hand back the input as-is so a) callers
		// that ignore errors still see something and b) the
		// failure mode is concentrated in the entry check.
		return strings.TrimRight(base, "/") + "/api/eval/plugins/" + url.PathEscape(pluginName) + "/webhook"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/eval/plugins/" + url.PathEscape(pluginName) + "/webhook"
	return u.String()
}

// timeoutOrDefault caps outbound automator calls. The default
// is 30s; the auth-flow check in plugin_tester.go is shorter
// (5s) because its probe is smaller work.
func (a *langsmithRuleAutomator) timeoutOrDefault() time.Duration {
	if a.cfg.Timeout > 0 {
		return a.cfg.Timeout
	}
	return 30 * time.Second
}

// headerOrDefault names the audit header. We use a single
// constant so the rule, the audit log, and the documentation
// agree. Operators renaming it would break the audit
// correlation, so we don't expose it as an env var.
func (a *langsmithRuleAutomator) headerOrDefault() string {
	if a.cfg.HeaderName != "" {
		return a.cfg.HeaderName
	}
	return "X-Nexus-Plugin"
}

// observability import is referenced indirectly via pluginStore
// wiring and kept for future log fields that surface the trace
// id the plugin is associated with. The blank assignment is
// removed by the linker in release builds but keeps `go vet`
// honest when this file is the only one to import observability
// outside the gateway path.
var _ = observability.Trace{}
