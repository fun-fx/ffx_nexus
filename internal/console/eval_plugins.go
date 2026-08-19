package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// EvalPluginSource depends on the runtime controller to absorb the
// registry the loader keeps. Mirrors EvalProfileSource so the wiring
// inside cmd/nexus/main.go follows the same pattern.
type EvalPluginSource interface {
	List(ctx context.Context, orgID string) ([]evalplugin.PluginRecord, error)
	Get(ctx context.Context, id string) (*evalplugin.PluginRecord, error)
	Save(ctx context.Context, r *evalplugin.PluginRecord) error
	Delete(ctx context.Context, id string) error
	// Lookup loads a plugin by metadata.name (Helm + DB merge); used
	// by the test-send handler so admins can resolve a plugin without
	// a database round-trip when the source is cluster-wide.
	Lookup(ctx context.Context, name string) (*evalplugin.PluginRecord, error)
}

// PluginWebhookReceiver is the inbound channel for webhook-mode
// plugins. Vendor services POST evaluation results to the
// /api/eval/plugins/:name/webhook endpoint.
type PluginWebhookReceiver interface {
	Webhook(name string, body io.Reader) error
}

// EvalPluginTester is an optional dependency for the test-send
// admin route. nil means the route answers 503.
//
// The `ref` argument is whichever path placeholder the operator
// supplied — the canonical metadata.name in the YAML manifest, or
// the row's database id. Implementations must resolve both forms
// before dispatching so the front-end never needs to know which
// identity the registry uses internally.
//
// Result.OK is true when the underlying vendor probe succeeded.
// Result.Message is the human-readable status the operator should
// see next to the plugin row (e.g. "Auth accepted by Langfuse in
// 124 ms" or the SDK error text on failure).
// orgID is threaded through so resolution happens inside one tenant. The
// {name} in the URL is client-supplied and plugin names are only unique per
// org, so an org-blind lookup let an admin of one org probe another org's
// plugin — using that org's stored vendor credentials, against that org's
// quota, and reporting the vendor's reply back to the wrong tenant.
type EvalPluginTester interface {
	Test(ctx context.Context, orgID, ref string) (Result, error)
}

// Result is the typed outcome of a PluginTester.Test call. The HTTP
// route emits this struct verbatim, so any change here should be
// mirrored on the client (PluginTestResult in web/src/api.ts).
type Result struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
}

// PluginManualFirer is the admin REST face of
// ExternalScheduler.FireManual / FireScheduled. The console calls it
// when an admin presses the "Run now" button on a manual- or
// scheduled-trigger plugin. The returned count tells the UI how many
// buffered traces (if any) were flushed; for manual plugins the
// buffer is normally empty because inline traces are not enqueued.
//
// orgID scopes the name resolution for the same reason as EvalPluginTester,
// with a sharper consequence: firing drains the plugin's buffered traces and
// dispatches them to its configured vendor. Resolving another org's plugin here
// forwards traffic to that org's vendor account, so the tenant is part of the
// call rather than something the implementation guesses.
type PluginManualFirer interface {
	FireManual(ctx context.Context, orgID, pluginName, trigger string) (int, error)
	FireScheduled(ctx context.Context, orgID, pluginName, trigger string) (int, error)
}

// pluginFireBody is the JSON contract the console sends when it
// presses "Run now". `Trigger` is an optional audit tag the operator
// can attach; the server falls back to "<admin-email>@<RFC3339>" when
// the field is empty so the audit log line still identifies the
// caller without any extra clicks in the UI.
type pluginFireBody struct {
	Trigger string `json:"trigger,omitempty"`
}

// pluginFireResponse is what /fire returns. `ok` is false when the
// plugin is missing, disabled, or has the wrong Send.Trigger for the
// endpoint that was hit (e.g. /scheduled against a manual plugin).
type pluginFireResponse struct {
	OK      bool   `json:"ok"`
	Count   int    `json:"count"`
	Message string `json:"message"`
}

// evalPluginBody is the wire shape the frontend speaks. We intentionally
// embed the raw YAML; the editor in EvalPlugins.tsx uses a textarea, so
// round-tripping the YAML verbatim preserves user comments and ordering.
type evalPluginBody struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	SpecYAML string `json:"spec_yaml"`
	Enabled  bool   `json:"enabled"`
}

type evalPluginPatch struct {
	SpecYAML *string `json:"spec_yaml,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}

func (s *Server) listEvalPlugins(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.evalPlugins == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugins disabled"})
		return
	}
	all, err := s.evalPlugins.List(r.Context(), orgID(r))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": all})
}

func (s *Server) createEvalPlugin(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalPlugins == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugins disabled"})
		return
	}
	var body evalPluginBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if _, err := evalplugin.Decode([]byte(body.SpecYAML)); err != nil {
		s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
		return
	}
	rec := &evalplugin.PluginRecord{
		// Plugin scope is compared against the *trace's* org id, which is
		// the virtual key's real org — never the "default" placeholder
		// orgID() returns for a request without an explicit X-Org-Id. A
		// row stamped with the placeholder therefore matched no traffic
		// and was skipped silently. Absent the header, store the row as
		// cluster-wide, which is what installing a plugin from the
		// console means. An explicit header still scopes it to that org.
		OrgID:    evalplugin.NormalizeOrgID(orgID(r)),
		Name:     body.Name,
		SpecYAML: body.SpecYAML,
		Enabled:  body.Enabled,
	}
	if err := s.evalPlugins.Save(r.Context(), rec); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("eval.plugin.create"), rec.ID, rec.Name)
	writeJSON(w, http.StatusCreated, rec)
}

// pluginByIDForOrg is the {id} counterpart of lookupPluginForOrg: it fetches a
// plugin record and refuses it if another org owns it.
//
// Get keys on the bare record id, so PATCH and DELETE were reachable across
// orgs. That matters more here than for most records, because a plugin manifest
// carries the vendor endpoint and the reference to the credential used to reach
// it. Disabling another team's plugin silently stops their traces from being
// scored, and there is no user-visible signal that scoring stopped.
//
// Returns ErrPluginNotFound for a foreign record, matching lookupPluginForOrg,
// so neither route can be used to confirm that an id exists.
func (s *Server) pluginByIDForOrg(ctx context.Context, orgID, id string) (*evalplugin.PluginRecord, error) {
	rec, err := s.evalPlugins.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, evalplugin.ErrPluginNotFound
	}
	if rec.OrgID != "" && evalplugin.NormalizeOrgID(rec.OrgID) != evalplugin.NormalizeOrgID(orgID) {
		return nil, evalplugin.ErrPluginNotFound
	}
	return rec, nil
}

func (s *Server) patchEvalPlugin(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalPlugins == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugins disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := s.pluginByIDForOrg(r.Context(), orgID(r), id)
	if err != nil {
		if errors.Is(err, evalplugin.ErrPluginNotFound) {
			s.fail(w, r, http.StatusNotFound, apierr.CodeNotFound, evalplugin.ErrPluginNotFound)
			return
		}
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	var patch evalPluginPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if patch.SpecYAML != nil {
		p, err := evalplugin.Decode([]byte(*patch.SpecYAML))
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, apierr.CodeInvalidRequest, err)
			return
		}
		existing.SpecYAML = *patch.SpecYAML
		// The registry keys on metadata.name, so a manifest rename has
		// to travel to the name column too. Leaving it stale made the
		// row unreachable through the by-name routes (test, keys,
		// webhook) while the registry answered under the new name.
		if n := strings.TrimSpace(p.Metadata.Name); n != "" {
			existing.Name = n
		}
	}
	if patch.Enabled != nil {
		existing.Enabled = *patch.Enabled
	}
	if err := s.evalPlugins.Save(r.Context(), existing); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("eval.plugin.update"), existing.ID, existing.Name)
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteEvalPlugin(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalPlugins == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugins disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := s.pluginByIDForOrg(r.Context(), orgID(r), id)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, apierr.CodeNotFound, err)
		return
	}
	if err := s.evalPlugins.Delete(r.Context(), id); err != nil {
		s.fail(w, r, http.StatusInternalServerError, apierr.CodeInternalError, err)
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("eval.plugin.delete"), id, existing.Name)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// pluginWebhook funnels incoming HTTP POSTs from vendor services to
// the PluginWebhookReceiver. Verification of vendor signatures is
// the receiver's responsibility — this handler only routes.
//
// We emit a typed status envelope (ok + accepted + optional message)
// on both success and failure so the React client can render the
// outcome in the per-row inline status instead of having to fall
// back to fetch's opaque `res.ok` boolean.
func (s *Server) pluginWebhook(w http.ResponseWriter, r *http.Request) {
	if s.pluginCollector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":      false,
			"message": "eval plugin collector disabled",
		})
		return
	}
	name := chi.URLParam(r, "name")
	defer r.Body.Close()
	if err := s.pluginCollector.Webhook(name, r.Body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":       true,
		"accepted": 1,
		"message":  "queued for evaluation",
	})
}

// pluginTest issues a vendor-specific connection probe. The
// underlying tester is responsible for choosing the cheapest
// authenticated request (LangSmith /api/v1/info, etc.) and
// returning a single error if anything in the chain is broken.
//
// The HTTP path placeholder is `{ref}` so the operator can drop in
// either the plugin's stable metadata.name ("langfuse-judge") or
// the database id assigned at Save. We resolve both into a single
// registry lookup and surface the friendly message that the vendor
// probe returned. The response shape (PluginTestResult) matches the
// TypeScript client interface exactly so the UI never has to guess
// which keys are present.
func (s *Server) pluginTest(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.pluginTester == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":      false,
			"message": "plugin tester not wired",
		})
		return
	}
	ref := chi.URLParam(r, "name")
	// The caller's org travels with the ref. Resolution happens inside the
	// tester, which is also what makes the authenticated vendor call, so that
	// is where the tenant boundary is enforced — one check at the place that
	// does the dangerous thing, rather than a second copy here that would go
	// stale. The console's job is to pass the session's org and never the
	// client's claim about it.
	res, err := s.pluginTester.Test(r.Context(), orgID(r), ref)
	if err != nil {
		// A failed probe is an application-level *result*, not a
		// transport failure, so it must be reported with 200. When we
		// answered 5xx here, every reverse proxy in front of the
		// console was entitled to discard our JSON body and
		// substitute its own error page: Cloudflare replaced it with
		// the branded "Bad gateway / Error code 502" HTML, which the
		// dashboard then reported as "auth or ingress likely
		// intercepted the request" — hiding the real message
		// ("plugin ... not found", "probe timed out", …) from the
		// operator. Keep the status 200 so the typed body always
		// survives the trip back to the browser.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         res.OK,
		"message":    res.Message,
		"latency_ms": res.LatencyMs,
	})
}

// pluginFireManual drains a manual-trigger plugin's per-plugin
// buffer and dispatches whatever was sitting in it via the shared
// Dispatcher. Inline traces are *not* enqueued for manual plugins,
// so the normal return is (0, nil); non-zero counts mean an admin
// previously flipped the trigger while the buffer was non-empty.
//
// The `trigger` field in the body is logged for audit but does not
// influence what gets sent; it exists so the operator can leave a
// note like `manual: weekly-smoke-run` that will appear in the
// scheduler's structured log line.
//
// The optional `?which=scheduled` query parameter switches the
// handler to the scheduled-trigger path. The body/schema stay the
// same; only the underlying PluginManualFirer method differs so the
// console can share one button-per-row UX across the two trigger
// shapes.
func (s *Server) pluginFireManual(w http.ResponseWriter, r *http.Request, user core.User) {
	if s.pluginManualFirer == nil {
		writeJSON(w, http.StatusServiceUnavailable, pluginFireResponse{
			OK:      false,
			Message: "manual-fire not wired",
		})
		return
	}
	ref := chi.URLParam(r, "name")
	trigger := ""
	if r.Body != nil {
		var body pluginFireBody
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		trigger = body.Trigger
	}
	if trigger == "" {
		// Default audit tag: the user's login plus the wall clock so
		// the operator can correlate runs without typing.
		trigger = user.Email + "@" + time.Now().UTC().Format(time.RFC3339)
	}
	mode := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("which")))
	var (
		count int
		err   error
	)
	switch mode {
	case "scheduled":
		count, err = s.pluginManualFirer.FireScheduled(r.Context(), orgID(r), ref, trigger)
	default:
		count, err = s.pluginManualFirer.FireManual(r.Context(), orgID(r), ref, trigger)
	}
	// The two error paths diverge on purpose: a manual-mode error is
	// a domain message the UI surfaces inline (the button stays in
	// place, count rounds to 0, no retry needed). A scheduled-mode
	// error usually means "this plugin is not actually a scheduled
	// plugin" — operator misused the affordance — so we surface a
	// 4xx so the typed JSON body survives Cloudflare's rewriting
	// while still letting the console render the message.
	if err != nil {
		status := http.StatusOK
		if mode == "scheduled" {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, pluginFireResponse{
			OK:      false,
			Count:   count,
			Message: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, pluginFireResponse{
		OK:      true,
		Count:   count,
		Message: fmt.Sprintf("%s fire for %q drained %d traces", modeOrManual(mode), ref, count),
	})
}

// modeOrManual turns the `?which=` query value into a human label
// for the success message. Empty string falls back to "manual" so the
// default (no query) endpoint still reads as "manual fire" in the
// console's status line.
func modeOrManual(mode string) string {
	switch mode {
	case "scheduled":
		return "scheduled"
	default:
		return "manual"
	}
}

// LangSmithRuleCreator is the optional dependency for the
// "Create automation rule" admin REST route. nil means the route
// answers 503 with a clear message so the React frontend can
// disable the button.
//
// Implementations are responsible for resolving the LangSmith
// API key from the plugin's key vault and computing the
// webhook URL — the interface only carries the inputs that
// change per request (session_id, display_name fragments).
//
// The split between "interface in console/" and "implementation
// in cmd/nexus/langsmith_rules.go" mirrors EvalPluginTester so
// tests can stub the seam with a deterministic response.
//
// orgID scopes name resolution: the implementation resolves the plugin's
// manifest *and its stored vendor API key* from the name, so an org-blind
// lookup let one tenant create rules in another tenant's LangSmith workspace
// using that tenant's credentials.
type LangSmithRuleCreator interface {
	CreateAutomationRule(ctx context.Context, orgID, pluginName, sessionID string) (AutomationRuleResult, error)
}

// AutomationRuleResult is what CreateAutomationRule returns. The
// fields map 1:1 to the typed JSON envelope the React client
// surfaces in the plugin row's status line so the UI never has
// to guess which keys are present.
//
// RuleID is the LangSmith-side rule id we stored as a tag on
// the local plugin row, so a follow-up delete is unambiguous.
// AlreadyConfigured is true when LangSmith returned 409 because
// the operator already created the rule by hand — the UI uses
// this to swap the help text to "you already have one" rather
// than "rule was created".
type AutomationRuleResult struct {
	OK                bool   `json:"ok"`
	RuleID            string `json:"rule_id,omitempty"`
	WebhookURL        string `json:"webhook_url,omitempty"`
	AlreadyConfigured bool   `json:"already_configured,omitempty"`
	Message           string `json:"message,omitempty"`
}

// SetLangSmithRuleCreator wires the vendor-side automator. nil
// disables the route; the Server uses the same nil-check pattern
// as pluginCollector to keep a missing dependency visible instead
// of crashing the boot.
func (s *Server) SetLangSmithRuleCreator(r LangSmithRuleCreator) {
	s.langsmithRuleCreator = r
}

// pluginCreateAutomationRule is the admin REST bridge between
// the React client and the LangSmith automator. Returns 200 on
// every flow path with the typed envelope in the body — the
// pattern documented in PR #197's "always return typed JSON" so
// the UI never has to fall back to "fetch failed" displays.
//
// The handler is intentionally tight: it does not log the API
// key (the resolver returns the resolved plaintext in memory
// only) and it does not echo the body back into the audit log
// (display_name carries the plugin name; nothing secret).
func (s *Server) pluginCreateAutomationRule(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.langsmithRuleCreator == nil {
		writeJSON(w, http.StatusServiceUnavailable, AutomationRuleResult{
			OK:      false,
			Message: "langsmith automation not wired",
		})
		return
	}
	ref := chi.URLParam(r, "name")
	var body struct {
		SessionID string `json:"session_id"`
	}
	// Body may be empty for the "no session_id override" path; in
	// that case the body decoder leaves the field unset and the
	// implementation falls back to whatever session the plugin
	// vault carries (today: passed through by the operator in the
	// UI). We do not 400 on missing session_id here because the
	// contract is "the LangSmithRuleCreator knows what it needs";
	// admin REST's job is to wire the click through, not validate.
	_ = json.NewDecoder(r.Body).Decode(&body)

	pluginName := strings.TrimSpace(ref)
	if pluginName == "" {
		writeJSON(w, http.StatusBadRequest, AutomationRuleResult{
			OK:      false,
			Message: "plugin name is required",
		})
		return
	}
	res, err := s.langsmithRuleCreator.CreateAutomationRule(r.Context(), orgID(r), pluginName, body.SessionID)
	if err != nil {
		// Already-Configured is a typed outcome (NOT a failure):
		// the operator already made the rule by hand, PATCH path
		// is what to suggest next. We surface it as 200 with the
		// envelope so the UI can branch without parsing strings.
		// Validation failures (missing session_id, etc.) are also
		// 200 + ok:false so the typed envelope survives.
		//
		// Either the implementation already populated Message on
		// the typed result (carrying a vendor-aware explanation)
		// or it left it blank, in which case we fall back to
		// err.Error() so a misbehaving implementation still
		// surfaces something useful. WebhookURL is always copied
		// through so the UI can show "this URL was advertised to
		// the vendor" on either path.
		msg := res.Message
		if msg == "" {
			msg = err.Error()
		}
		writeJSON(w, http.StatusOK, AutomationRuleResult{
			OK:                false,
			AlreadyConfigured: res.AlreadyConfigured,
			WebhookURL:        res.WebhookURL,
			Message:           msg,
		})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), core.AuditAction("eval.langsmith.rule.create"), res.RuleID, pluginName)
	writeJSON(w, http.StatusOK, res)
}
