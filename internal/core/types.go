// Package core holds the control-plane domain: organizations, virtual keys,
// provider credentials, and the Postgres-backed store. It degrades gracefully:
// when Postgres is not configured, the gateway runs in zero-dependency mode.
package core

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// VirtualKey is a downstream credential apps present to the gateway. Beyond
// auth, it is the tenancy axis that observability, evals, and routing bind to.
type VirtualKey struct {
	ID            string    `json:"id"`
	OrgID         string    `json:"org_id"`
	UserID        string    `json:"user_id,omitempty"` // owning user (BYOK); empty = org-level
	Name          string    `json:"name"`
	KeyPrefix     string    `json:"key_prefix"`
	KeyLast4      string    `json:"key_last4"`
	AllowedModels []string  `json:"allowed_models"`
	RPMLimit      int       `json:"rpm_limit"`
	MonthlyBudget float64   `json:"monthly_budget_usd"`
	MinQuality    float64   `json:"min_quality_score"`
	Revoked       bool      `json:"revoked"`
	CreatedAt     time.Time `json:"created_at"`
}

// ProviderCredential is an upstream provider API key, stored encrypted. The
// plaintext secret is returned only once, at creation.
type ProviderCredential struct {
	ID          string     `json:"id"`
	OrgID       string     `json:"org_id"`
	UserID      string     `json:"user_id,omitempty"` // owning user (BYOK); empty = org-level
	Provider    string     `json:"provider"`
	Name        string     `json:"name"`
	BaseURL     string     `json:"base_url,omitempty"`
	SecretLast4 string     `json:"secret_last4"`
	Enabled     bool       `json:"enabled"`
	CreatedAt   time.Time  `json:"created_at"`
	RotatedAt   *time.Time `json:"rotated_at,omitempty"`
}

// User is a human identity within an org. Virtual keys and BYOK provider
// credentials may be owned by a user. Passwords are bcrypt-hashed; the hash is
// never serialized to API responses.
type User struct {
	ID            string    `json:"id"`
	OrgID         string    `json:"org_id"`
	Email         string    `json:"email"`
	Role          string    `json:"role"` // "admin" | "member"
	EnforceLimits bool      `json:"enforce_limits"`
	CreatedAt     time.Time `json:"created_at"`
}

// Roles for users.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// keyAlphabet for the random body of a virtual key (Crockford-ish base32).
var keyEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// GenerateVirtualKey returns a new plaintext virtual key plus its display
// prefix and last-4. Format: "nxs_live_<28+ random chars>".
func GenerateVirtualKey() (plaintext, prefix, last4 string) {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	body := keyEncoding.EncodeToString(buf)
	plaintext = "nxs_live_" + body
	// Prefix shown in lists: scheme + first 4 of the body.
	prefix = "nxs_live_" + body[:4]
	last4 = plaintext[len(plaintext)-4:]
	return plaintext, prefix, last4
}

// GenerateSessionToken returns a new high-entropy opaque session token. Only
// its hash is stored; the plaintext is sent to the client as a cookie.
func GenerateSessionToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "nxs_sess_" + keyEncoding.EncodeToString(buf)
}

// Last4 returns the last 4 characters of a secret for display.
func Last4(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
