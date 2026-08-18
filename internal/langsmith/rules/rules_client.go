// Package rules drives Nexus's outbound automation of LangSmith
// configuration. The goal is to remove the operator's manual
// step in the LangSmith UI (open an Automation Rule, point it at
// Nexus's collector URL, paste a JSON template) — every step
// LangSmith exposes via its public REST API is replicated here
// so that pressing "Create automation rule" in the console
// produces the same vendor-side resource as clicking through
// the UI.
package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
)

// Endpoints live under the LangSmith base host. cloudLangHost is
// the public SaaS; self-host operators override per-plugin via
// the EvalPlugin endpoint.
const (
	defaultCloudBase = "https://api.smith.langchain.com"
	rulesPath        = "/api/v1/runs/rules"
)

// Errors returned by the client. Each is exported so admin REST
// handlers can map them to typed HTTP responses.
var (
	ErrUnauthorized   = errors.New("langsmith: x-api-key rejected (401)")
	ErrInvalidRequest = errors.New("langsmith: request body invalid (422)")
	ErrConflict       = errors.New("langsmith: rule already exists for this session")
	ErrUpstream       = errors.New("langsmith: upstream failure")
)

// Webhook describes one webhook action attached to a rule. The
// shape mirrors the LangSmith RunRulesWebhookSchema verbatim, so
// future vendor changes (e.g. adding `method`) flow through with
// the smallest diff.
type Webhook struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// CreateRuleRequest is the body Nexus POSTs to /api/v1/runs/rules.
// SamplingRate is 1.0 by default — operators expect every scored
// trace to land in Nexus. SessionID is the LangSmith project UUID
// associated with the tenant (admin pre-flighted it in the
// console). DisplayName is auto-generated as
// "Nexus <plugin-name> webhook" so that re-running with
// different Nexus plugins produces distinct LangSmith rules
// rather than overwriting each other.
type CreateRuleRequest struct {
	DisplayName  string    `json:"display_name"`
	SessionID    string    `json:"session_id,omitempty"`
	IsEnabled    bool      `json:"is_enabled"`
	SamplingRate float64   `json:"sampling_rate"`
	Webhooks     []Webhook `json:"webhooks"`
}

// Rule is the parsed /api/v1/runs/rules response. It carries just
// enough fields for Nexus to track which vendor-side rule mirrors
// the local plugin (the rule ID).
type Rule struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	IsEnabled   bool   `json:"is_enabled"`
}

// Client is the small HTTP client used by admin REST handlers and
// tests. The vendored URL is constructed per-call from Endpoint
// so a self-hosted deployment Just Works without extra plumbing.
type Client struct {
	// Endpoint is the LangSmith base URL. Defaults to the public
	// SaaS when empty. Per-plugin override arrives via the
	// EvalPlugin spec.service.endpoint field.
	Endpoint string
	// APIKey is the resolved LangSmith API key (x-api-key header).
	// Empty key is refused by CreateRule; admin REST handlers
	// resolve it from the plugin key vault before calling.
	APIKey string
	// HTTPClient lets tests swap in a deterministic transport. nil
	// selects a sensible default with a 15s timeout.
	HTTPClient *http.Client
}

// New constructs a Client with the cloud endpoint filled in if
// Endpoint is empty. APIKey must come from the caller — bundling
// it from cfg would be the same anti-pattern we already fixed in
// the OTLP/JSON ingest path.
func New(endpoint, apiKey string) *Client {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultCloudBase
	}
	endpoint = strings.TrimRight(endpoint, "/")
	return &Client{
		Endpoint: endpoint,
		APIKey:   strings.TrimSpace(apiKey),
		// Tenant class: endpoint comes from the plugin manifest an org admin
		// installed, so it is a destination the caller chose.
		HTTPClient: egress.Client(egress.Tenant, 15*time.Second),
	}
}

// CreateRule POSTs /api/v1/runs/rules with one webhook action
// pointing at Nexus's collector endpoint. The collector URL is
// computed by the caller (admin REST handler has the host +
// plugin name in hand) and passed in as webhookURL — the rules
// package deliberately stays ignorant of how Nexus advertises
// its own URL.
//
// Idempotency: if a rule with the same DisplayName already
// exists (which the operator might have created manually
// before installing this feature), LangSmith returns a 409-style
// error we surface as ErrConflict. The handler maps this to a
// typed 200 {ok:false, message:"..."} so the React UI can
// surface it without parsing strings.
func (c *Client) CreateRule(ctx context.Context, req CreateRuleRequest, webhookURL string) (Rule, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return Rule{}, errors.New("langsmith: APIKey is empty")
	}
	if req.SessionID == "" {
		return Rule{}, &validationError{msg: "session_id is required"}
	}
	if webhookURL == "" {
		return Rule{}, &validationError{msg: "webhook URL is required"}
	}
	if req.DisplayName == "" {
		return Rule{}, &validationError{msg: "display_name is required"}
	}
	if req.SamplingRate <= 0 || req.SamplingRate > 1 {
		return Rule{}, &validationError{msg: "sampling_rate must be in (0,1]"}
	}
	if len(req.Webhooks) == 0 {
		req.Webhooks = []Webhook{{URL: webhookURL}}
	} else {
		// Always overwrite the first webhook's URL with the
		// Nexus-side URL we computed. Operators may not realise
		// that the rule stores a URL per webhook; instructing
		// the UI to show a preview alongside the Auto-create
		// button keeps the "what would Nexus send?" mental
		// model honest.
		req.Webhooks[0].URL = webhookURL
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Rule{}, fmt.Errorf("langsmith: marshal: %w", err)
	}
	url := c.Endpoint + rulesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Rule{}, fmt.Errorf("langsmith: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	resp, err := c.client().Do(httpReq)
	if err != nil {
		return Rule{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return Rule{}, fmt.Errorf("%w: %s", ErrUnauthorized, strings.TrimSpace(string(respBody)))
	case resp.StatusCode == http.StatusUnprocessableEntity,
		resp.StatusCode == http.StatusBadRequest:
		return Rule{}, fmt.Errorf("%w: %s", ErrInvalidRequest, strings.TrimSpace(string(respBody)))
	case resp.StatusCode == http.StatusConflict:
		// LangSmith's idempotency surface — they reject duplicate
		// display_name within a session. Surface as ErrConflict so
		// the handler can return a typed "already configured" UI
		// state rather than a generic 5xx.
		return Rule{}, fmt.Errorf("%w: %s", ErrConflict, strings.TrimSpace(string(respBody)))
	case resp.StatusCode/200 != 1:
		return Rule{}, fmt.Errorf("%w: HTTP %d: %s", ErrUpstream, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out Rule
	if err := json.Unmarshal(respBody, &out); err != nil {
		return Rule{}, fmt.Errorf("langsmith: decode response: %w", err)
	}
	if out.ID == "" {
		// Vendor returned 2xx but did not include an id. This
		// has not been observed against the live API but we
		// defend against it: a rule we cannot reference is a
		// rule we cannot delete when the plugin is removed.
		return Rule{}, fmt.Errorf("langsmith: response missing id: %s", strings.TrimSpace(string(respBody)))
	}
	return out, nil
}

// AppendRuleRequest is the body Nexus PATCHes to an existing
// rule. We use it to add the webhook to a rule the operator
// hand-crafted before this feature shipped, so that an
// upgrade-in-place experience works without the operator
// recreating their UI-built rule.
type AppendRuleRequest struct {
	Webhooks []Webhook `json:"webhooks"`
}

// AppendWebhook PATCHes an existing rule to add (or replace) a
// webhook action. Required for the "operator had a pre-existing
// rule before installing this build" upgrade path; without it,
// the only action was CreateRule which 409'd on duplicate.
//
// We PATCH (not PUT) so other fields are preserved. The vendor
// uses PATCH /api/v1/runs/rules/{rule_id} for partial updates;
// the body shape is identical to RunRulesUpdateSchema.
func (c *Client) AppendWebhook(ctx context.Context, ruleID string, hook Webhook) error {
	if strings.TrimSpace(c.APIKey) == "" {
		return errors.New("langsmith: APIKey is empty")
	}
	if ruleID == "" {
		return &validationError{msg: "rule_id is required"}
	}
	if hook.URL == "" {
		return &validationError{msg: "webhook URL is required"}
	}
	body, err := json.Marshal(AppendRuleRequest{Webhooks: []Webhook{hook}})
	if err != nil {
		return fmt.Errorf("langsmith: marshal: %w", err)
	}
	url := c.Endpoint + rulesPath + "/" + ruleID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("langsmith: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.APIKey)
	resp, err := c.client().Do(httpReq)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrUnauthorized, strings.TrimSpace(string(respBody)))
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("langsmith: rule_id %q not found: %s", ruleID, strings.TrimSpace(string(respBody)))
	case resp.StatusCode/200 != 1:
		return fmt.Errorf("%w: HTTP %d: %s", ErrUpstream, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// client returns the configured HTTP client. Single seam so a
// test can swap transport once and have CreateRule /
// AppendWebhook both see it.
func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	// Never http.DefaultClient: it has no timeout and no destination policy, so
	// a zero-valued Client would silently opt out of both.
	return egress.Client(egress.Tenant, 15*time.Second)
}

// validationError is an internal marker so admin REST handlers
// can return HTTP 400 on this exact class of failure rather
// than 500. We use a distinct type rather than wrapping
// ErrInvalidRequest so the boundary stays explicit.
type validationError struct{ msg string }

func (v *validationError) Error() string { return "langsmith: " + v.msg }

// IsValidation reports whether err is a client-side validation
// failure. Exported so admin REST handlers can branch on
// errors.Is(err, ErrValidation) without importing this package's
// unexported type.
var ErrValidation = errors.New("langsmith: invalid request")

// AsValidation wraps the internal sentinel for errors.Is.
// Returns nil when err is not a *validationError.
func AsValidation(err error) error {
	var v *validationError
	if errors.As(err, &v) {
		return fmt.Errorf("%w: %s", ErrValidation, v.msg)
	}
	return nil
}
