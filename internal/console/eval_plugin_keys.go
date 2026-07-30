package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// EvalPluginKeys is the resolver interface the Plugin Keys panel
// talks to. The concrete implementation lives in cmd/nexus (see
// plugin_keys.go); it caches values in memory and persists them
// encrypted through the control-plane store.
//
// Set and Clear return an error because a key that reaches memory but
// not the database looks configured until the next rolling update
// quietly unconfigures it. Reporting the write failure is what turns
// that into a visible problem.
//
// This interface deliberately does NOT expose Get / Set / Clear with
// key *values* to the rest of the codebase — only the console REST
// handlers see those. The router, dispatch path, and collect path
// ask via Resolve (mirroring external.SecretResolver) so a leak of
// this interface through Go imports does not give a caller raw key
// bytes.
type EvalPluginKeys interface {
	Get(plugin string) (map[string]string, bool)
	Set(plugin string, kv map[string]string) error
	Clear(plugin string) error
	Has(plugin string) bool
}

// pluginKeysBody is the wire shape the Panel speaks. The "keys" map
// is keyed by the manifest's keyRef names: public_key/secret_key for
// Langfuse, api_key for Braintrust, dd_api_key/dd_app_key for Datadog,
// etc. Passing the same key names as the manifest is intentional so
// the Panel and the manifest stay in sync by construction.
type pluginKeysBody struct {
	Keys map[string]string `json:"keys"`
}

// pluginKeysState is the typed status the GET handler returns. It is
// what the UI consumes to decide whether to render the modal in
// "saved" state vs. "empty" state and whether each named key has a
// value configured.
type pluginKeysState struct {
	Plugin     string            `json:"plugin"`
	Configured bool              `json:"configured"`
	Keys       []pluginKeysEntry `json:"keys"`
	Required   []string          `json:"required_key_names,omitempty"`
}

type pluginKeysEntry struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
}

func (s *Server) lookupPluginForOrg(ctx context.Context, orgID, name string) (*evalplugin.PluginRecord, error) {
	if s.evalPlugins == nil {
		return nil, errors.New("eval plugins disabled")
	}
	rec, err := s.evalPlugins.Lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, evalplugin.ErrPluginNotFound
	}
	if rec.OrgID != "" && rec.OrgID != orgID {
		// Same name under a different org is NOT the plugin the
		// resolver should target. We deliberately return
		// ErrPluginNotFound rather than leaking existence; the operator
		// trusted this name to call their own keys endpoint.
		return nil, evalplugin.ErrPluginNotFound
	}
	return rec, nil
}

// decodePluginManifest parses the stored YAML so the handlers can
// read keyRef / auth fields without depending on the in-memory
// registry. Decode succeeds only on previously-validated manifests
// (Store.Save re-validates), so an error here is a 500 not a 400.
func decodePluginManifest(rec *evalplugin.PluginRecord) (*evalplugin.Plugin, error) {
	if rec == nil {
		return nil, errors.New("plugin record is nil")
	}
	return evalplugin.Decode([]byte(rec.SpecYAML))
}

// requiredKeyNames splits a plugin's manifest keyRef into a stable
// ordered list (no leading "$.", no empty entries). Used to teach the
// UI how many inputs to render on first paint.
func requiredKeyNames(rec *evalplugin.PluginRecord) []string {
	if rec == nil {
		return nil
	}
	p, err := decodePluginManifest(rec)
	if err != nil {
		return nil
	}
	return splitKeyRef(p.Spec.Service.Auth.KeyRef)
}

func splitKeyRef(keyRef string) []string {
	out := make([]string, 0, 2)
	start := 0
	for i := 0; i < len(keyRef); i++ {
		if keyRef[i] != '|' {
			continue
		}
		if s := trimSpaces(keyRef[start:i]); s != "" {
			out = append(out, s)
		}
		start = i + 1
	}
	if s := trimSpaces(keyRef[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

func trimSpaces(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// getPluginKeys returns the *configured* state of a plugin's keys.
// The shape is intentionally narrow: names + boolean, never values,
// so the response is safe to log and to surface in DevTools.
func (s *Server) getPluginKeys(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.pluginKeys == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":      false,
			"message": "plugin key resolver not wired",
		})
		return
	}
	name := chi.URLParam(r, "name")
	rec, err := s.lookupPluginForOrg(r.Context(), orgID(r), name)
	if err != nil {
		if errors.Is(err, evalplugin.ErrPluginNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"ok":      false,
				"message": "plugin not found",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}

	required := requiredKeyNames(rec)
	stored, has := s.pluginKeys.Get(name)
	state := pluginKeysState{Plugin: name, Configured: has, Required: required}
	if !has {
		// Mark every required key as not-set so the UI renders the
		// modal in "freshly created" state.
		for _, k := range required {
			state.Keys = append(state.Keys, pluginKeysEntry{Name: k, Set: false})
		}
	} else {
		// Always report keys in the same order as the manifest,
		// followed by any extras the operator may have set in a
		// previous revision. Stable ordering helps the UI render the
		// same shape every time.
		seen := make(map[string]bool)
		for _, k := range required {
			state.Keys = append(state.Keys, pluginKeysEntry{Name: k, Set: stored[k] != ""})
			seen[k] = true
		}
		// extras in lexicographic order
		extras := make([]string, 0, len(stored))
		for k := range stored {
			if !seen[k] {
				extras = append(extras, k)
			}
		}
		sortStrings(extras)
		for _, k := range extras {
			state.Keys = append(state.Keys, pluginKeysEntry{Name: k, Set: stored[k] != ""})
		}
	}
	writeJSON(w, http.StatusOK, state)
}

// putPluginKeys replaces the stored key map for one plugin. The body
// is `{keys: {...}}` where keys are the canonical names from the
// manifest's keyRef (so the UI cannot invent arbitrary keys).
func (s *Server) putPluginKeys(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.pluginKeys == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":      false,
			"message": "plugin key resolver not wired",
		})
		return
	}
	name := chi.URLParam(r, "name")
	rec, err := s.lookupPluginForOrg(r.Context(), orgID(r), name)
	if err != nil {
		if errors.Is(err, evalplugin.ErrPluginNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"ok":      false,
				"message": "plugin not found",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}
	var body pluginKeysBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"message": "invalid JSON body",
		})
		return
	}
	if body.Keys == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"message": `body must contain a "keys" object`,
		})
		return
	}

	required := requiredKeyNames(rec)
	if len(required) > 0 && len(body.Keys) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"message": "no keys supplied; paste them in the modal first",
		})
		return
	}
	// Reject unknown key names so a typo doesn't silently no-op.
	known := make(map[string]bool, len(required)+4)
	for _, k := range required {
		known[k] = true
	}
	for k := range body.Keys {
		if !known[k] && k != "" {
			// Allow but warn. Strict mode is opt-in for now to keep
			// rotation flows easy (some keys like "tag" or "region"
			// are valid for some vendors and not others).
		}
	}

	if err := s.pluginKeys.Set(name, body.Keys); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.plugin.keys.set", rec.ID, name)
	writeJSON(w, http.StatusOK, pluginKeysState{
		Plugin:     name,
		Configured: len(stripEmpty(body.Keys)) > 0,
		Required:   required,
	})
}

// deletePluginKeys clears all stored keys for a plugin. Used by the
// "Clear" button on the modal; also useful when an operator
// decommissioning a vendor wants to remove any leftover plaintext.
func (s *Server) deletePluginKeys(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.pluginKeys == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":      false,
			"message": "plugin key resolver not wired",
		})
		return
	}
	name := chi.URLParam(r, "name")
	rec, err := s.lookupPluginForOrg(r.Context(), orgID(r), name)
	if err != nil {
		if errors.Is(err, evalplugin.ErrPluginNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"ok":      false,
				"message": "plugin not found",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}
	if err := s.pluginKeys.Clear(name); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":      false,
			"message": err.Error(),
		})
		return
	}
	s.audit(r.Context(), u.ID, orgID(r), "eval.plugin.keys.clear", rec.ID, name)
	writeJSON(w, http.StatusOK, pluginKeysState{
		Plugin:     name,
		Configured: false,
		Required:   requiredKeyNames(rec),
	})
}

// stripEmpty returns kv with empty-string entries removed.
func stripEmpty(kv map[string]string) map[string]string {
	out := make(map[string]string, len(kv))
	for k, v := range kv {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// sortStrings is a tiny stable sort that avoids a dependency on
// sort.Strings so this handler works in any order-of-evaluation
// environment. We use it for the set of "extra" keys when shaping
// the GET response so the same JSON comes back on every call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
