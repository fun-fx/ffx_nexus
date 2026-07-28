package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
type EvalPluginTester interface {
	Test(name string) error
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
		OrgID:    orgID(r),
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
		if _, err := evalplugin.Decode([]byte(*patch.SpecYAML)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		existing.SpecYAML = *patch.SpecYAML
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
func (s *Server) pluginWebhook(w http.ResponseWriter, r *http.Request) {
	if s.pluginCollector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "eval plugin collector disabled"})
		return
	}
	name := chi.URLParam(r, "name")
	defer r.Body.Close()
	if err := s.pluginCollector.Webhook(name, r.Body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// pluginTest issues a vendor-specific connection probe. The
// underlying tester is responsible for choosing the cheapest
// authenticated request (LangSmith /api/v1/info, etc.) and
// returning a single error if anything in the chain is broken.
func (s *Server) pluginTest(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.pluginTester == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "plugin tester not wired"})
		return
	}
	name := chi.URLParam(r, "name")
	if err := s.pluginTester.Test(name); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
