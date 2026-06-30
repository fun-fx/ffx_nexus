package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Embeddings handles POST /v1/embeddings. It dispatches to an embed-capable
// provider based on the requested model id (precision match or "provider/model"
// prefix). Supports all OpenAI-compatible input shapes (string, []string,
// []int, [][]int) by preserving the union as raw JSON.
//
// Auth + rate limit + per-user credential resolution are shared with the
// /v1/chat/completions pipeline (the same middleware chain wraps the mux).
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	var req EmbeddingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if len(req.Input) == 0 || strings.TrimSpace(string(req.Input)) == "null" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return
	}

	// Honor a "provider/model" prefix even for embeddings so a multi-tenant
	// deployment can disambiguate; ResolveEmbedding handles both this and an
	// exact id match.
	ep, ok := h.registry.ResolveEmbedding(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model_not_found",
			"no embeddings provider registered for model "+req.Model)
		return
	}

	if req.EncodingFormat != "" && req.EncodingFormat != "float" && req.EncodingFormat != "base64" {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"encoding_format must be \"float\" or \"base64\"")
		return
	}

	// Caller credential resolution (BYOK): shared with chat completions so a
	// user's stored OpenAI key is reused here when present.
	ctx := r.Context()
	if h.credResolver != nil && h.keyMode != KeyModeShared {
		orgID := OrgIDFrom(ctx)
		userID := UserIDFrom(ctx)
		if cred, found, err := h.credResolver.Resolve(ctx, orgID, userID, ep.Name()); err == nil && found {
			ctx = WithCallerCredential(ctx, CallerCredential{
				Secret:  cred.Secret,
				BaseURL: cred.BaseURL,
				Source:  cred.Source,
			})
		}
	}

	resp, err := ep.Embed(ctx, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error",
			"embeddings upstream error: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
