package evals

import (
	"context"
	"errors"

	"github.com/ffxnexus/nexus/internal/core"
)

// StoreSecretLookup adapts core.Store.ResolveCredential into the
// SecretLookup interface so the eval runtime can ride along the same
// credential precedence the gateway already uses for routed calls
// (BYOK over org). Inline secrets are loaded into the resolver's
// in-memory map via RegisterInline / RevokeInline rather than this
// lookup — the inline table lives in eval_credentials and the
// runtime controller drives the snapshot updates.
//
// Concrete methodology:
//
//  1. Org requests userID="" so the resolver asks the store to
//     return the first credential with user_id IS NULL (after
//     enabling the filter on user_id in the wire).
//
//  2. User requests keep userID = the trace owner (or the
//     EvalEndpoint's KeyRef when explicit), again letting the
//     store's BYOK precedence kick in.
//
//  3. Inline lives in the resolver's sharded map; the lookup's
//     Inline method is a typed error stub so the resolver falls
//     back to its in-memory entries.
type StoreSecretLookup struct {
	Store *core.Store
	OrgID string
}

// NewStoreSecretLookup returns a SecretLookup driven by core.Store.
// OrgID defaults to "default" when empty; single-tenant builds don't
// have to wire it.
func NewStoreSecretLookup(s *core.Store, orgID string) *StoreSecretLookup {
	if orgID == "" {
		orgID = "default"
	}
	return &StoreSecretLookup{Store: s, OrgID: orgID}
}

// Org returns the first enabled org-level credential matching the
// provider. The store's hot-path crawler already prioritises the
// newest credential as a tiebreaker, so the resolver can be repeated
// per-call without harming throughput (the worker pool stays small).
func (l *StoreSecretLookup) Org(ctx context.Context, provider string) (string, error) {
	if l == nil || l.Store == nil {
		return "", errors.New("store not configured")
	}
	cred, _, err := l.Store.ResolveCredential(ctx, l.OrgID, "", provider)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return "", ErrSecretNotFound
		}
		return "", err
	}
	return cred.Secret, nil
}

// User returns the requested user's credential. The store's
// ResolverCredential honours BYOK precedence, so a user-owned row
// beats an org row of the same provider — exactly the behaviour
// the eval profile semantics demand when OwnerUserID is set.
func (l *StoreSecretLookup) User(ctx context.Context, provider, userID string) (string, error) {
	if l == nil || l.Store == nil {
		return "", errors.New("store not configured")
	}
	if userID == "" {
		// Empty user is treated as org lookup for ergonomics so a
		// profile created without a KeyRef doesn't trip "missing
		// user" while the resolve path is being debugged.
		return l.Org(ctx, provider)
	}
	cred, _, err := l.Store.ResolveCredential(ctx, l.OrgID, userID, provider)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return "", ErrSecretNotFound
		}
		return "", err
	}
	return cred.Secret, nil
}

// Inline is intentionally a stub. The resolver relies on its
// in-memory sharded map populated by the runtime controller; a
// store-backed variant would require introducing a new table and
// migration, which is out of scope for PR #136 (the eval profile
// writes are still owner=UPDATEd into eval_credentials in PR #137).
func (l *StoreSecretLookup) Inline(_ context.Context, _ string) (string, error) {
	return "", errors.New("inline resolution uses resolver in-memory map; populate via RegisterInline")
}
