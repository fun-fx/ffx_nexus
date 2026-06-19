package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/core/crypto"
	"github.com/ffxnexus/nexus/internal/observability"
)

// sessionTTL is how long a console login session stays valid.
const sessionTTL = 7 * 24 * time.Hour

// sessionCookie is the cookie name carrying the opaque session token.
const sessionCookie = "nexus_session"

type userCtxKey struct{}

// currentUser returns the authenticated user from the request context, if any.
func currentUser(r *http.Request) (core.User, bool) {
	u, ok := r.Context().Value(userCtxKey{}).(core.User)
	return u, ok
}

// withUser attaches a session middleware: it resolves the session cookie to a
// user and stores it in the context. It does not reject unauthenticated
// requests; route guards (requireUser/requireAdmin) do that.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil {
			next.ServeHTTP(w, r)
			return
		}
		token := sessionTokenFrom(r)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		u, err := s.store.UserBySession(r.Context(), token)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), userCtxKey{}, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireUser wraps a handler, returning 401 when there is no authenticated user.
func (s *Server) requireUser(fn func(http.ResponseWriter, *http.Request, core.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		fn(w, r, u)
	}
}

// requireAdmin wraps a handler, returning 403 for non-admins.
func (s *Server) requireAdmin(fn func(http.ResponseWriter, *http.Request, core.User)) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request, u core.User) {
		if u.Role != core.RoleAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
			return
		}
		fn(w, r, u)
	})
}

func sessionTokenFrom(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	// Allow a bearer token too, for API clients/tests.
	return bearerToken(r)
}

func bearerToken(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

// --- Auth endpoints ---

const minPasswordLen = 8

func (s *Server) authConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"signup_enabled": s.allowSignup && s.store != nil,
		"sso_enabled":    s.SSOEnabled(),
		"sso_label":      s.SSOLabel(),
	})
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

func validateEmail(email string) bool {
	email = strings.TrimSpace(email)
	at := strings.LastIndex(email, "@")
	if at < 1 || at >= len(email)-1 {
		return false
	}
	return strings.Contains(email[at+1:], ".")
}

func validatePassword(password string) bool {
	return len(password) >= minPasswordLen
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type registerRequest struct {
	Email          string `json:"email"`
	Password       string `json:"password"`
	Provider       string `json:"provider,omitempty"`
	ProviderName   string `json:"provider_name,omitempty"`
	ProviderSecret string `json:"provider_secret,omitempty"`
	KeyName        string `json:"key_name,omitempty"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.requireStore(w) {
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	token, u, err := s.store.Authenticate(r.Context(), orgID(r), req.Email, req.Password, sessionTTL)
	if errors.Is(err, core.ErrInvalidCredentials) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid email or password"})
		return
	}
	if err != nil {
		s.log.Error("login failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "login failed"})
		return
	}
	setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.allowSignup {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "registration disabled"})
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if !validateEmail(req.Email) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid email"})
		return
	}
	if !validatePassword(req.Password) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "password must be at least 8 characters",
		})
		return
	}
	if req.ProviderSecret != "" && req.Provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "provider is required when provider_secret is set",
		})
		return
	}
	if !s.requireStore(w) {
		return
	}

	u, err := s.store.CreateUser(r.Context(), orgID(r), req.Email, req.Password, core.RoleMember)
	if errors.Is(err, core.ErrEmailTaken) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "email already registered"})
		return
	}
	if err != nil {
		s.log.Error("register failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "registration failed"})
		return
	}

	resp := map[string]any{"user": u}
	var warnings []string

	if req.Provider != "" && req.ProviderSecret != "" {
		_, credErr := s.store.CreateCredential(r.Context(), u.OrgID, u.ID, req.Provider, req.ProviderName, "", req.ProviderSecret)
		switch {
		case errors.Is(credErr, crypto.ErrNoMasterKey):
			warnings = append(warnings, "provider key not stored: set NEXUS_MASTER_KEY to enable BYOK credentials")
		case credErr != nil:
			s.log.Error("register credential failed", "err", credErr, "user", u.ID)
			warnings = append(warnings, "provider key not stored: registration failed to save credential")
		default:
			s.reloadCredentials(r.Context())
			keyName := strings.TrimSpace(req.KeyName)
			if keyName == "" {
				keyName = "default"
			}
			_, plaintext, keyErr := s.store.CreateVirtualKey(r.Context(), u.OrgID, u.ID, keyName, nil, 0, 0, 0)
			if keyErr != nil {
				s.log.Error("register virtual key failed", "err", keyErr, "user", u.ID)
				warnings = append(warnings, "virtual key not created: add one from the console")
			} else {
				resp["virtual_key"] = plaintext
			}
		}
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}

	token, authed, err := s.store.Authenticate(r.Context(), orgID(r), req.Email, req.Password, sessionTTL)
	if err != nil {
		s.log.Error("register auto-login failed", "err", err)
		writeJSON(w, http.StatusCreated, resp)
		return
	}
	setSessionCookie(w, token)
	resp["user"] = authed
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		if token := sessionTokenFrom(r); token != "" {
			_ = s.store.Logout(r.Context(), token)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, u core.User) {
	writeJSON(w, http.StatusOK, u)
}

// --- Self-service usage (member's own data) ---
//
// /api/me/stats, /api/me/traces, /api/me/quality all scope to the caller's
// user_id so members see only their own traffic, not the org-wide aggregate.
// Mirrors the admin /api/stats|/traces|/users/quality responses; the only
// difference is the WHERE user_id = ? filter (see internal/observability/reader.go).

func (s *Server) myStats(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, observability.Stats{})
		return
	}
	window := time.Hour
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			window = d
		}
	}
	st, err := s.reader.WindowStats(r.Context(), window, u.ID)
	if err != nil {
		s.log.Error("my stats query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) myTraces(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	traces, err := s.reader.RecentTraces(r.Context(), limit, u.ID)
	if err != nil {
		s.log.Error("my traces query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, traces)
}

// myQuality returns the caller's own rolling quality/spend aggregate as a
// single-element array (matches the /api/users/quality row shape so the
// client can reuse the UserQuality type).
func (s *Server) myQuality(w http.ResponseWriter, r *http.Request, u core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []observability.UserQuality{})
		return
	}
	window := time.Hour
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			window = d
		}
	}
	rows, err := s.reader.UserQualitySummary(r.Context(), window, u.ID)
	if err != nil {
		s.log.Error("my quality query failed", "err", err)
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

type updateMeRequest struct {
	EnforceLimits *bool `json:"enforce_limits"`
}

// updateMe lets a user toggle their own Nexus-side budget/RPM enforcement.
func (s *Server) updateMe(w http.ResponseWriter, r *http.Request, u core.User) {
	var req updateMeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.EnforceLimits != nil {
		if err := s.store.SetEnforceLimits(r.Context(), u.ID, *req.EnforceLimits); err != nil {
			s.writeStoreErr(w, err, "update failed")
			return
		}
		u.EnforceLimits = *req.EnforceLimits
	}
	writeJSON(w, http.StatusOK, u)
}

// --- User management (admin) ---

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, _ core.User) {
	users, err := s.store.ListUsers(r.Context(), orgID(r))
	if err != nil {
		s.log.Error("list users failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	if users == nil {
		users = []core.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, _ core.User) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Email == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email and password are required"})
		return
	}
	u, err := s.store.CreateUser(r.Context(), orgID(r), req.Email, req.Password, req.Role)
	if errors.Is(err, core.ErrEmailTaken) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "email already registered"})
		return
	}
	if err != nil {
		s.log.Error("create user failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create failed"})
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// userQualityRow is a per-user quality/spend aggregate with the user's email
// resolved for display.
type userQualityRow struct {
	UserID     string  `json:"user_id"`
	Email      string  `json:"email"`
	AvgQuality float64 `json:"avg_quality"`
	PassRate   float64 `json:"pass_rate"`
	Samples    int64   `json:"samples"`
	CostUSD    float64 `json:"cost_usd"`
	Requests   int64   `json:"requests"`
}

// userQuality returns per-user rolling quality + spend (admin only). This is the
// eval differentiator: quality per user, not just spend per key (design §9).
func (s *Server) userQuality(w http.ResponseWriter, r *http.Request, _ core.User) {
	if s.reader == nil {
		writeJSON(w, http.StatusOK, []userQualityRow{})
		return
	}
	window := time.Hour
	if q := r.URL.Query().Get("window"); q != "" {
		if d, err := time.ParseDuration(q); err == nil {
			window = d
		}
	}
	rows, err := s.reader.UserQualitySummary(r.Context(), window, "")
	if err != nil {
		s.log.Error("user quality query failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	// Resolve emails for display (best-effort; falls back to the id).
	emails := map[string]string{}
	if users, uErr := s.store.ListUsers(r.Context(), orgID(r)); uErr == nil {
		for _, u := range users {
			emails[u.ID] = u.Email
		}
	}
	out := make([]userQualityRow, 0, len(rows))
	for _, q := range rows {
		out = append(out, userQualityRow{
			UserID:     q.UserID,
			Email:      emails[q.UserID],
			AvgQuality: q.AvgQuality,
			PassRate:   q.PassRate,
			Samples:    q.Samples,
			CostUSD:    q.CostUSD,
			Requests:   q.Requests,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, _ core.User) {
	id := chi.URLParam(r, "id")
	if err := s.store.DeleteUser(r.Context(), orgID(r), id); err != nil {
		s.writeStoreErr(w, err, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Self-service virtual keys (/api/me/keys) ---

func (s *Server) listMyKeys(w http.ResponseWriter, r *http.Request, u core.User) {
	keys, err := s.store.ListVirtualKeysForUser(r.Context(), u.OrgID, u.ID)
	if err != nil {
		s.log.Error("list my keys failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	if keys == nil {
		keys = []core.VirtualKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) createMyKey(w http.ResponseWriter, r *http.Request, u core.User) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	vk, plaintext, err := s.store.CreateVirtualKey(r.Context(), u.OrgID, u.ID, req.Name, req.AllowedModels, req.RPMLimit, req.MonthlyBudget, req.MinQuality)
	if err != nil {
		s.log.Error("create my key failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": vk, "secret": plaintext})
}

func (s *Server) revokeMyKey(w http.ResponseWriter, r *http.Request, u core.User) {
	id := chi.URLParam(r, "id")
	// Scope the revoke to the user's own keys by checking ownership first.
	keys, err := s.store.ListVirtualKeysForUser(r.Context(), u.OrgID, u.ID)
	if err != nil {
		s.writeStoreErr(w, err, "revoke failed")
		return
	}
	owned := false
	for _, k := range keys {
		if k.ID == id {
			owned = true
			break
		}
	}
	if !owned {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err := s.store.RevokeVirtualKey(r.Context(), u.OrgID, id); err != nil {
		s.writeStoreErr(w, err, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- Self-service BYOK credentials (/api/me/credentials) ---

func (s *Server) listMyCredentials(w http.ResponseWriter, r *http.Request, u core.User) {
	creds, err := s.store.ListCredentialsForUser(r.Context(), u.OrgID, u.ID)
	if err != nil {
		s.log.Error("list my credentials failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	if creds == nil {
		creds = []core.ProviderCredential{}
	}
	writeJSON(w, http.StatusOK, creds)
}

func (s *Server) createMyCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	var req createCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Provider == "" || req.Secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and secret are required"})
		return
	}
	cred, err := s.store.CreateCredential(r.Context(), u.OrgID, u.ID, req.Provider, req.Name, req.BaseURL, req.Secret)
	if errors.Is(err, crypto.ErrNoMasterKey) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "credential encryption disabled: set NEXUS_MASTER_KEY to store provider keys",
		})
		return
	}
	if err != nil {
		s.log.Error("create my credential failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create failed"})
		return
	}
	s.reloadCredentials(r.Context())
	writeJSON(w, http.StatusCreated, cred)
}

func (s *Server) rotateMyCredential(w http.ResponseWriter, r *http.Request, u core.User) {
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
	cred, err := s.store.RotateUserCredential(r.Context(), u.OrgID, u.ID, id, req.Secret)
	if errors.Is(err, crypto.ErrNoMasterKey) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "credential encryption disabled: set NEXUS_MASTER_KEY to rotate provider keys",
		})
		return
	}
	if err != nil {
		s.writeStoreErr(w, err, "rotate failed")
		return
	}
	s.reloadCredentials(r.Context())
	writeJSON(w, http.StatusOK, cred)
}

func (s *Server) deleteMyCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	id := chi.URLParam(r, "id")
	if err := s.store.DeleteUserCredential(r.Context(), u.OrgID, u.ID, id); err != nil {
		s.writeStoreErr(w, err, "delete failed")
		return
	}
	s.reloadCredentials(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
