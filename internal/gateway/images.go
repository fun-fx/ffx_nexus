package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Images handles POST /v1/images/generations. Dispatches to a provider that
// implements ImageGenerationProvider (currently only OpenAI). The model
// field is optional — an empty value resolves to the provider's default
// image model (dall-e-3 on OpenAI).
//
// Auth + rate limit + per-user credential resolution (BYOK) follow the same
// pattern as chat / embeddings / moderations. The admin's NEXUS key never
// leaks — the user's stored OpenAI key (if any) is used instead.
func (h *Handler) Images(w http.ResponseWriter, r *http.Request) {
	var req ImageGenerationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}

	ip, resolvedModel, ok := h.registry.ResolveImage(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found",
			"no image provider registered for model "+req.Model)
		return
	}
	if req.Model == "" {
		req.Model = resolvedModel
	}

	ctx := r.Context()
	if h.credResolver != nil && h.keyMode != KeyModeShared {
		orgID := OrgIDFrom(ctx)
		userID := UserIDFrom(ctx)
		if cred, found, err := h.credResolver.Resolve(ctx, orgID, userID, ip.Name()); err == nil && found {
			ctx = WithCallerCredential(ctx, CallerCredential{
				Secret:  cred.Secret,
				BaseURL: cred.BaseURL,
				Source:  cred.Source,
			})
		}
	}

	resp, err := ip.GenerateImages(ctx, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error",
			"image generation upstream error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
