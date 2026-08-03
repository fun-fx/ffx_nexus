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
type EvalPluginTester interface {
	Test(ctx context.Context, ref string) (Result, error)
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
// ExternalScheduler.FireManual. The console calls it when an admin
// presses the "Run now" button on a manual-trigger plugin. The
// returned count tells the UI how many buffered traces (if any)
// were flushed; for manual plugins the buffer is normally empty
// because inline traces are not enqueued.
type PluginManualFirer interface {
	FireManual(ctx context.Context, pluginName, trigger string) (int, error)
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.plugin.create", rec.ID, rec.Name)
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) patchEvalPlugin(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalPlugins == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugins disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := s.evalPlugins.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, evalplugin.ErrPluginNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.plugin.update", existing.ID, existing.Name)
	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) deleteEvalPlugin(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.evalPlugins == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugins disabled"})
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := s.evalPlugins.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err := s.evalPlugins.Delete(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.plugin.delete", id, existing.Name)
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
	res, err := s.pluginTester.Test(r.Context(), ref)
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
func (s *Server) pluginFireManual(w http.ResponseWriter, r *http.Request, user core.User) {
	if s.pluginManualFirer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":      false,
			"message": "manual-fire not wired",
		})
		return
	}
	ref := chi.URLParam(r, "name")
	trigger := ""
	if r.Body != nil {
		var body struct {
			Trigger string `json:"trigger"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		trigger = body.Trigger
	}
	if trigger == "" {
		// Default audit tag: the user's login plus the wall clock so
		// the operator can correlate runs without typing.
		trigger = user.Email + "@" + time.Now().UTC().Format(time.RFC3339)
	}
	count, err := s.pluginManualFirer.FireManual(r.Context(), ref, trigger)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": err.Error(),
			"count":   count,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("manual fire for %q drained %d traces", ref, count),
		"count":   count,
	})
}
