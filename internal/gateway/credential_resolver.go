package gateway

import (
	"context"
	"sync"
	"time"

	nexusurlpolicy "github.com/ffxnexus/nexus/internal/urlpolicy"
)

// ResolvedCredential is a decrypted upstream secret plus metadata, as returned
// by a CredentialSource.
type ResolvedCredential struct {
	Secret  string
	BaseURL string
	Source  string // "user" | "org"
	ID      string // credential id, for cache keying / invalidation
}

// CredentialSource resolves a single enabled credential for (org, user,
// provider), honoring BYOK precedence (user-owned beats org-level). It returns
// ErrNoCredential-style behavior via found=false. Implemented by the control
// plane store; kept as an interface so the gateway package does not depend on
// core.
type CredentialSource interface {
	ResolveCredential(ctx context.Context, orgID, userID, provider string) (cred ResolvedCredential, found bool, err error)
}

// KeyMode selects how the gateway resolves upstream provider keys per request.
type KeyMode int

const (
	// KeyModeShared uses the process-wide env/org keys for everyone (legacy).
	KeyModeShared KeyMode = iota
	// KeyModeBYOK prefers each caller's own stored key, falling back to the
	// shared keys when the caller has none for the target provider.
	KeyModeBYOK
	// KeyModeStrictBYOK requires a per-user key; callers without one are rejected.
	KeyModeStrictBYOK
)

// ParseKeyMode maps a config string to a KeyMode (defaults to shared).
func ParseKeyMode(s string) KeyMode {
	switch s {
	case "byok":
		return KeyModeBYOK
	case "strict_byok", "strict-byok":
		return KeyModeStrictBYOK
	default:
		return KeyModeShared
	}
}

// CredentialResolver wraps a CredentialSource with a short-TTL in-memory cache
// so the AES-GCM decrypt + DB hit do not repeat on every request. Entries are
// keyed by (org, user, provider) and hold the plaintext secret in memory only.
type CredentialResolver struct {
	src CredentialSource
	ttl time.Duration
	// allowlist is the operator-supplied CIDR list used by urlpolicy at the
	// dial-time gate. An empty value disables private-network destinations
	// entirely. The same value is reused across Resolve() calls; setting
	// it once at startup avoids a per-call CSV parse.
	allowlist string

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	cred    ResolvedCredential
	found   bool
	expires time.Time
}

// NewCredentialResolver builds a resolver with the given cache TTL. A zero or
// negative ttl disables caching (always hits the source).
func NewCredentialResolver(src CredentialSource, ttl time.Duration, allowlist string) *CredentialResolver {
	return &CredentialResolver{
		src:       src,
		ttl:       ttl,
		allowlist: allowlist,
		cache:     make(map[string]cacheEntry),
	}
}

// validateAtDialTime applies the urlpolicy gate at resolve-time.
// Empty BaseURL passes — that is the case for shared env keys
// that do not override the upstream base. The save-time gate (in
// Store.CreateCredential) is responsible for catching any bad
// value the moment the operator saves it.
func validateAtDialTime(raw, allowlist string) error {
	if raw == "" {
		return nil
	}
	return nexusurlpolicy.Validate(raw, allowlist)
}

// Resolve returns the credential for (org, user, provider), using the cache when
// fresh. The found return distinguishes "no credential" from an error.
//
// On hit, the cached BaseURL is re-validated against the save-time
// gate (urlpolicy.Validate) before being returned. A pre-validate
// cache hit that no longer passes the gate returns a 502 to the
// caller rather than dialling an endpoint the operator has since
// rejected. The gate runs once per Resolve; the cache lookup
// itself stays fast.
func (cr *CredentialResolver) Resolve(ctx context.Context, orgID, userID, provider string) (ResolvedCredential, bool, error) {
	if cr == nil || cr.src == nil {
		return ResolvedCredential{}, false, nil
	}
	key := orgID + "\x00" + userID + "\x00" + provider
	if cr.ttl > 0 {
		cr.mu.RLock()
		e, ok := cr.cache[key]
		cr.mu.RUnlock()
		if ok && time.Now().Before(e.expires) {
			if err := validateAtDialTime(e.cred.BaseURL, cr.allowlist); err != nil {
				return ResolvedCredential{}, false, err
			}
			return e.cred, e.found, nil
		}
	}
	cred, found, err := cr.src.ResolveCredential(ctx, orgID, userID, provider)
	if err != nil {
		return ResolvedCredential{}, false, err
	}
	if cred.BaseURL != "" {
		if err := validateAtDialTime(cred.BaseURL, cr.allowlist); err != nil {
			return ResolvedCredential{}, false, err
		}
	}
	if cr.ttl > 0 {
		cr.mu.Lock()
		cr.cache[key] = cacheEntry{cred: cred, found: found, expires: time.Now().Add(cr.ttl)}
		cr.mu.Unlock()
	}
	return cred, found, nil
}

// Invalidate clears the cache (e.g. after a credential create/rotate/delete) so
// the next request re-resolves. Cheap and safe to call on any change.
func (cr *CredentialResolver) Invalidate() {
	if cr == nil {
		return
	}
	cr.mu.Lock()
	cr.cache = make(map[string]cacheEntry)
	cr.mu.Unlock()
}
