package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/core/crypto"
)

// orgID resolves the tenant for a request. Multi-tenant auth (SSO/RBAC) is a
// commercial feature; OSS defaults to a single "default" org.
func orgID(r *http.Request) string {
	if v := r.Header.Get("X-Org-Id"); v != "" {
		return v
	}
	return "default"
}

// requireStore writes a 503 when the control-plane store is unavailable.
func (s *Server) requireStore(w http.ResponseWriter) bool {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "control plane disabled: set NEXUS_POSTGRES_URL to enable key/credential management",
		})
		return false
	}
	return true
}

// --- Virtual keys ---

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	keys, err := s.store.ListVirtualKeys(r.Context(), orgID(r))
	if err != nil {
		s.log.Error("list keys failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	if keys == nil {
		keys = []core.VirtualKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

type createKeyRequest struct {
	Name          string   `json:"name"`
	UserID        string   `json:"user_id"` // optional: bind the key to a user (BYOK)
	AllowedModels []string `json:"allowed_models"`
	RPMLimit      int      `json:"rpm_limit"`
	MonthlyBudget float64  `json:"monthly_budget_usd"`
	MinQuality    float64  `json:"min_quality_score"`
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.requireStore(w) {
		return
	}
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	vk, plaintext, err := s.store.CreateVirtualKey(r.Context(), orgID(r), u.ID, req.UserID, req.Name, req.AllowedModels, req.RPMLimit, req.MonthlyBudget, req.MinQuality)
	if err != nil {
		s.log.Error("create key failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create failed"})
		return
	}
	// The plaintext key is returned exactly once, here.
	writeJSON(w, http.StatusCreated, map[string]any{"key": vk, "secret": plaintext})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.requireStore(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.store.RevokeVirtualKey(r.Context(), orgID(r), u.ID, id); err != nil {
		s.writeStoreErr(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- Provider credentials ---

func (s *Server) listCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	creds, err := s.store.ListCredentials(r.Context(), orgID(r))
	if err != nil {
		s.log.Error("list credentials failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	if creds == nil {
		creds = []core.ProviderCredential{}
	}
	writeJSON(w, http.StatusOK, creds)
}

type createCredentialRequest struct {
	Provider string                  `json:"provider"`
	Name     string                  `json:"name"`
	BaseURL  string                  `json:"base_url"`
	Secret   string                  `json:"secret"`
	Models   core.CredentialModels   `json:"models,omitempty"`
}

func (s *Server) createCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.requireStore(w) {
		return
	}
	var req createCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Provider == "" || req.Secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and secret are required"})
		return
	}
	cred, err := s.store.CreateCredential(r.Context(), orgID(r), u.ID, "", req.Provider, req.Name, req.BaseURL, req.Secret, req.Models)
	if errors.Is(err, crypto.ErrNoMasterKey) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "credential encryption disabled: set NEXUS_MASTER_KEY (32-byte base64/hex) to store provider keys",
		})
		return
	}
	if err != nil {
		s.log.Error("create credential failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create failed"})
		return
	}
	// Response never includes the plaintext secret (only last4).
	writeJSON(w, http.StatusCreated, cred)
}

func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.requireStore(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.store.DeleteCredential(r.Context(), orgID(r), u.ID, id); err != nil {
		s.writeStoreErr(w, err, "delete failed")
		return
	}
	s.reloadCredentials(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type rotateCredentialRequest struct {
	Secret string `json:"secret"`
}

// rotateCredential swaps the stored secret for a credential in place. The new
// secret is re-encrypted; the credential keeps its ID and references. After a
// successful rotation the gateway's in-memory providers are reloaded so the new
// secret takes effect without a restart.
func (s *Server) rotateCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	if !s.requireStore(w) {
		return
	}
	var req rotateCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "secret is required"})
		return
	}
	id := chi.URLParam(r, "id")
	cred, err := s.store.RotateCredential(r.Context(), orgID(r), u.ID, id, req.Secret)
	if errors.Is(err, crypto.ErrNoMasterKey) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "credential encryption disabled: set NEXUS_MASTER_KEY (32-byte base64/hex) to rotate provider keys",
		})
		return
	}
	if err != nil {
		s.writeStoreErr(w, err, "rotate failed")
		return
	}
	s.reloadCredentials(r.Context())
	// Response never includes the plaintext secret (only last4 + rotated_at).
	writeJSON(w, http.StatusOK, cred)
}

// reloadCredentials invokes the registered hot-reload hook (if any) so provider
// adapters pick up credential changes without a restart. Best-effort.
func (s *Server) reloadCredentials(ctx context.Context) {
	if s.reload != nil {
		s.reload(ctx)
	}
}

func (s *Server) writeStoreErr(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, core.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	s.log.Error(msg, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
}

// listAudit returns recent audit_log entries for the caller's org. Admin only
// (the route is wrapped with requireAdmin in Mux). Supports ?limit=, ?action=,
// ?user_id= (filters by actor), and ?since= (RFC3339 or duration like "24h").
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, _ core.User) {
	if !s.requireStore(w) {
		return
	}
	q := r.URL.Query()
	opts := core.AuditListOptions{
		Action:  q.Get("action"),
		ActorID: q.Get("user_id"),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a non-negative integer"})
			return
		}
		opts.Limit = n
	}
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			opts.Since = t
		} else if d, err := time.ParseDuration(v); err == nil {
			opts.Since = time.Now().Add(-d)
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC3339 or a duration like 24h"})
			return
		}
	}
	entries, err := s.store.ListAudit(r.Context(), orgID(r), opts)
	if err != nil {
		s.log.Error("list audit failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	if entries == nil {
		entries = []core.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
