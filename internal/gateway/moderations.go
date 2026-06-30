package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Moderations handles POST /v1/moderations. Dispatches to a provider that
// implements ModerationsProvider (currently only OpenAI). The model field is
// optional — an empty value is resolved to the first registered model's
// default (omni-moderation-latest on OpenAI).
//
// Auth + rate limit + per-user credential resolution (BYOK) follow the same
// pattern as the embeddings handler.
func (h *Handler) Moderations(w http.ResponseWriter, r *http.Request) {
	var req ModerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Input) == 0 || strings.TrimSpace(string(req.Input)) == "null" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return
	}

	mp, resolvedModel, ok := h.registry.ResolveModeration(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found",
			"no moderation provider registered for model "+req.Model)
		return
	}
	if req.Model == "" {
		// Echo the resolved model back to the caller so the response surfaces
		// the model that actually ran (matches OpenAI's behavior).
		req.Model = resolvedModel
	}

	ctx := r.Context()
	if h.credResolver != nil && h.keyMode != KeyModeShared {
		orgID := OrgIDFrom(ctx)
		userID := UserIDFrom(ctx)
		if cred, found, err := h.credResolver.Resolve(ctx, orgID, userID, mp.Name()); err == nil && found {
			ctx = WithCallerCredential(ctx, CallerCredential{
				Secret:  cred.Secret,
				BaseURL: cred.BaseURL,
				Source:  cred.Source,
			})
		}
	}

	resp, err := mp.Moderate(ctx, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error",
			"moderation upstream error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
