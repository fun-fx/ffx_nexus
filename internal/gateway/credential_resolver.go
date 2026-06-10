package gateway

import (
	"context"
	"sync"
	"time"
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
func NewCredentialResolver(src CredentialSource, ttl time.Duration) *CredentialResolver {
	return &CredentialResolver{
		src:   src,
		ttl:   ttl,
		cache: make(map[string]cacheEntry),
	}
}

// Resolve returns the credential for (org, user, provider), using the cache when
// fresh. The found return distinguishes "no credential" from an error.
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
			return e.cred, e.found, nil
		}
	}
	cred, found, err := cr.src.ResolveCredential(ctx, orgID, userID, provider)
	if err != nil {
		return ResolvedCredential{}, false, err
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
