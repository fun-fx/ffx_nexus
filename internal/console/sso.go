package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ffxnexus/nexus/internal/config"
	"github.com/ffxnexus/nexus/internal/core"
)

// ssoStateCookie is the cookie name carrying the one-shot CSRF state for
// the OIDC flow. It is HttpOnly + SameSite=Lax, scoped to /api/auth/sso
// (only the callback ever needs to read it), with a 5-minute TTL.
const ssoStateCookie = "nexus_sso_state"

const ssoStateTTL = 5 * time.Minute

// ssoClient wires the OIDC provider and the OAuth2 config so the
// /login and /callback handlers can stay small. It is created lazily by
// SetSSO; the field stays nil on the Server when SSO is not configured,
// which both gates the routes and the authConfig response.
type ssoClient struct {
	cfg      config.SSOConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

func newSSOClient(ctx context.Context, cfg config.SSOConfig) (*ssoClient, error) {
	if !cfg.Enabled() {
		return nil, errors.New("sso: config not enabled")
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("sso: discover issuer: %w", err)
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	return &ssoClient{
		cfg:      cfg,
		provider: provider,
		verifier: verifier,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
	}, nil
}

// newState returns a 32-byte random value, base64url encoded, that we
// round-trip through a cookie and the auth-code URL.
func newState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func setSSOStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookie,
		Value:    state,
		Path:     "/api/auth/sso",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ssoStateTTL),
	})
}

func clearSSOStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     ssoStateCookie,
		Value:    "",
		Path:     "/api/auth/sso",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func ssoStateFrom(r *http.Request) string {
	if c, err := r.Cookie(ssoStateCookie); err == nil {
		return c.Value
	}
	return ""
}

// ssoLogin redirects the user-agent to the IdP authorize endpoint. The
// state we just generated is the only thing that ties the eventual
// callback to this particular user-agent; we keep it in an HttpOnly
// cookie so JS cannot tamper with it.
func (s *Server) ssoLogin(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	state, err := newState()
	if err != nil {
		s.log.Error("sso state generation failed", "err", err)
		http.Error(w, "sso unavailable", http.StatusInternalServerError)
		return
	}
	setSSOStateCookie(w, state)
	authURL := s.sso.oauth.AuthCodeURL(state, oidc.Nonce(generateNonce()))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// generateNonce returns an opaque value used in the ID-token nonce claim.
// It is informational only (we do not store it on the server side), but
// a non-empty nonce forces the IdP to include a nonce we can sanity-check.
func generateNonce() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// ssoCallback exchanges the code for tokens, validates the ID token,
// then either links the verified identity to an existing user (by email)
// or JIT-provisions a new member. On success it issues a Nexus session
// cookie and bounces the browser to /.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		desc := r.URL.Query().Get("error_description")
		s.renderSSOError(w, "SSO login failed: "+errParam, desc)
		return
	}
	want := ssoStateFrom(r)
	got := r.URL.Query().Get("state")
	clearSSOStateCookie(w)
	if want == "" || got == "" || want != got {
		s.renderSSOError(w, "Invalid SSO state", "the state token did not match; retry from the sign-in page")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		s.renderSSOError(w, "Missing authorization code", "")
		return
	}

	tok, err := s.sso.oauth.Exchange(r.Context(), code)
	if err != nil {
		s.log.Error("sso token exchange failed", "err", err)
		s.renderSSOError(w, "SSO token exchange failed", err.Error())
		return
	}

	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok {
		s.renderSSOError(w, "SSO response missing id_token", "")
		return
	}
	idTok, err := s.sso.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		s.log.Error("sso id_token verify failed", "err", err)
		s.renderSSOError(w, "SSO id_token verification failed", err.Error())
		return
	}

	var claims struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idTok.Claims(&claims); err != nil {
		s.renderSSOError(w, "SSO id_token claims unreadable", err.Error())
		return
	}
	// Some IdPs gate the email scope differently. If the id_token has no
	// email, fall back to the userinfo endpoint (which Keycloak serves at
	// /realms/{realm}/protocol/openid-connect/userinfo).
	if claims.Email == "" {
		if ui, uiErr := s.sso.provider.UserInfo(r.Context(), oauth2.StaticTokenSource(tok)); uiErr == nil {
			_ = ui.Claims(&claims)
		}
	}
	if claims.Email == "" {
		s.renderSSOError(w, "SSO did not return an email", "the upstream IdP must include the 'email' claim/scope")
		return
	}
	if !claims.EmailVerified {
		s.renderSSOError(w, "Email not verified at IdP", "Nexus only links verified identities; verify the user's email at the IdP")
		return
	}

	orgID := orgID(r)
	email := strings.TrimSpace(strings.ToLower(claims.Email))
	subject := claims.Sub
	if subject == "" {
		// Without a stable subject we cannot re-link later. Refuse rather
		// than bind to an unstable identifier.
		s.renderSSOError(w, "SSO did not return a subject", "")
		return
	}

	u, err := s.ssoLinkOrCreate(r.Context(), orgID, email, subject, idTok.Issuer)
	if err != nil {
		s.log.Error("sso user provisioning failed", "err", err, "email", email)
		s.renderSSOError(w, "SSO provisioning failed", err.Error())
		return
	}
	s.audit(r.Context(), u.ID, orgID, "sso.login", u.ID, email)

	token, err := s.store.CreateSession(r.Context(), u.ID, sessionTTL)
	if err != nil {
		s.log.Error("sso session create failed", "err", err)
		s.renderSSOError(w, "Could not create session", err.Error())
		return
	}
	setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusFound)
}

// ssoLinkOrCreate implements the email-link JIT policy: an incoming
// (provider, subject, email) is either bound to the existing user with
// that email (if any) or used to provision a fresh member account. In
// both cases the (provider, subject) tuple is recorded so future logins
// skip the email lookup.
func (s *Server) ssoLinkOrCreate(ctx context.Context, orgID, email, subject, issuer string) (core.User, error) {
	if s.store == nil {
		return core.User{}, errors.New("control plane disabled")
	}
	providerName := issuerProvider(issuer)

	// 1. Already bound by (org, provider, subject)? Re-login, no email match needed.
	if u, err := s.findBySSOIdentity(ctx, orgID, providerName, subject); err == nil {
		return u, nil
	}

	// 2. Email matches an existing user — link and continue.
	if u, err := s.store.GetUserByEmail(ctx, orgID, email); err == nil {
		if err := s.store.LinkSSOIdentity(ctx, u.ID, providerName, subject, issuer); err != nil {
			return core.User{}, fmt.Errorf("link existing user: %w", err)
		}
		u, _ = s.store.GetUser(ctx, u.ID)
		return u, nil
	}

	// 3. Brand new user: JIT provision as 'member'. The password hash is
	//    a random unguessable value so password login is impossible; the
	//    user is expected to come back via SSO. This matches the policy
	//    of treating SSO-only accounts as a different class.
	randomSecret, err := randomPasswordPlaceholder()
	if err != nil {
		return core.User{}, err
	}
	// SSO JIT provisioning is a system action (no caller yet), so actorID
	// is empty and Store.Audit stores "system" in the audit_log.actor column.
	u, err := s.store.CreateUser(ctx, "", orgID, email, randomSecret, core.RoleMember)
	if err != nil {
		return core.User{}, err
	}
	if err := s.store.LinkSSOIdentity(ctx, u.ID, providerName, subject, issuer); err != nil {
		// Best-effort cleanup: the user row is harmless on its own and
		// the next SSO login for the same subject will find it by email.
		s.log.Warn("sso link after create failed", "err", err, "user", u.ID)
	}
	u, _ = s.store.GetUser(ctx, u.ID)
	return u, nil
}

func (s *Server) findBySSOIdentity(ctx context.Context, orgID, provider, subject string) (core.User, error) {
	var u core.User
	err := s.store.Pool().QueryRow(ctx, `
		SELECT id, org_id, email, role, enforce_limits, created_at
		FROM users
		WHERE org_id = $1 AND sso_provider = $2 AND sso_subject = $3`,
		orgID, provider, subject,
	).Scan(&u.ID, &u.OrgID, &u.Email, &u.Role, &u.EnforceLimits, &u.CreatedAt)
	return u, err
}

// issuerProvider turns an OIDC issuer URL into a short, stable label
// suitable for storage (e.g. https://kc.example/realms/cozy -> kc:cozy).
// The same IdP signing in via different paths should produce the same
// provider string, so users do not get re-bound under a different key.
func issuerProvider(issuer string) string {
	issuers := strings.TrimRight(issuer, "/")
	if u, err := url.Parse(issuers); err == nil && u.Host != "" {
		host := strings.Split(u.Host, ":")[0]
		path := strings.Trim(u.Path, "/")
		if path != "" {
			return host + ":" + path
		}
		return host
	}
	return issuers
}

// randomPasswordPlaceholder returns a 32-byte random string used as a
// placeholder bcrypt input for SSO-only users. Login via /api/auth/login
// will never succeed because the user does not know this value; SSO is
// the only path back in.
func randomPasswordPlaceholder() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// renderSSOError shows a minimal HTML error page on the callback so the
// user sees something readable in their browser; the console SPA
// itself is the only other surface that talks to these endpoints.
func (s *Server) renderSSOError(w http.ResponseWriter, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	body := "<!doctype html><meta charset=utf-8><title>SSO error</title>"
	body += "<style>body{font-family:system-ui;max-width:520px;margin:64px auto;color:#222}"
	body += "h1{color:#b00;font-size:18px}code{background:#f4f4f4;padding:2px 6px;border-radius:4px}</style>"
	body += "<h1>" + htmlEscape(title) + "</h1>"
	if detail != "" {
		body += "<p>" + htmlEscape(detail) + "</p>"
	}
	body += `<p><a href="/">Back to console</a></p>`
	_, _ = w.Write([]byte(body))
}

func htmlEscape(s string) string {
	var b strings.Builder
	_ = json.NewEncoder(&b).Encode(s)
	// json.Encoder wraps in quotes; strip them.
	out := b.String()
	if len(out) >= 2 {
		return out[1 : len(out)-1]
	}
	return out
}
