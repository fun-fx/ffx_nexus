package evals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// SecretSourceKind controls where a profile's API key is pulled from.
// Mirrors evals.KeySource so the runtime layer doesn't depend on JSON
// shapes — the source comes pre-parsed off EvalProfile.Endpoint.
//
// Inline secrets are decrypted here and dropped from memory as soon
// as the resolver returns the bytes to the evaluator (an Evaluate
// call copies the string into the request body and is free to let GC
// reclaim the buffer).
type SecretSourceKind int

const (
	SourceOrg SecretSourceKind = iota + 1
	SourceUser
	SourceInline
	SourceBuiltin // heuristics
)

// SecretLookup is the dependency the runtime wires in PR #136. It
// resolves a profile's referenced credential against the durable tables
// (provider_credentials / eval_credentials). Implementations should be
// concurrency-safe — multiple worker goroutines call Resolve in
// parallel from evaluate().
//
// The user variant takes the trace's UserID and only returns the key
// when the trace owner matches the profile owner; the org variant
// always returns; inline resolves against the encrypted secrets
// table by stable key_ref token.
type SecretLookup interface {
	Org(ctx context.Context, provider string) (string, error)
	User(ctx context.Context, provider, userID string) (string, error)
	Inline(ctx context.Context, keyRef string) (string, error)
}

// ErrSecretNotFound is returned by SecretLookup when the requested
// credential row does not exist (deleted, expired, or never
// imported). The worker swallows this in profile dispatch because
// missing credentials must NOT 500 the gateway hot path.
var ErrSecretNotFound = errors.New("eval secret not found")

// resolverConfig clamps the runtime behaviour. Defaults in NewResolver.
type resolverConfig struct {
	// OrgProvider is the fallback provider name when the profile uses
	// key_source=org but the profile's Endpoint has no provider
	// attached. Defaults to "openai" so historic single-tenant
	// integration doesn't disappear silently.
	OrgProvider string
	// LookupTimeout caps any single SecretLookup call so a slow DB
	// degrades to "no key" rather than blocking eval dispatch.
	LookupTimeout time.Duration
}

func (c *resolverConfig) applyDefaults() {
	if c.OrgProvider == "" {
		c.OrgProvider = "openai"
	}
	if c.LookupTimeout == 0 {
		c.LookupTimeout = 250 * time.Millisecond
	}
}

// Resolver is the SecretResolver that PR #136 wires into the worker.
// Each call is on a worker goroutine; the implementation must not
// hold any process-wide locks for longer than the smallest of the
// three lookups.
//
// Inline secrets are kept in an in-memory map populated at startup
// from the eval_credentials table; the runtime controller PATCHes
// the resolver with `RegisterInline(key, plaintext)` whenever a row
// is created or rotated. The map is sharded (shardedInline, 32
// shards) so a busy resolver doesn't trend the global mutex hot.
type Resolver struct {
	lookup SecretLookup
	cfg    resolverConfig
	log    func(format string, args ...any)

	shards [32]*inlineShard
	once   sync.Once
}

type inlineShard struct {
	mu      sync.RWMutex
	entries map[string]inlineEntry
}

type inlineEntry struct {
	plain   string
	expires time.Time
}

// NewResolver builds a Resolver binding to the supplied SecretLookup
// and using the optional logger for "secret-not-found" warnings.
// When log is nil a discard logger is used.
func NewResolver(lookup SecretLookup, opts ...func(*resolverConfig)) *Resolver {
	cfg := resolverConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	cfg.applyDefaults()
	r := &Resolver{
		lookup: lookup,
		cfg:    cfg,
		log:    func(string, ...any) {},
	}
	for i := range r.shards {
		r.shards[i] = &inlineShard{entries: make(map[string]inlineEntry)}
	}
	return r
}

// SetLogger replaces the (otherwise discard) logger.
func (r *Resolver) SetLogger(log func(format string, args ...any)) {
	if log != nil {
		r.log = log
	}
}

// RegisterInline places a decrypted plaintext in the resolver's
// in-memory sharded map under `keyRef`. Called when a console
// POST/PATCH lands a new secret so the inline credentials table
// update reaches the worker without a process restart.
func (r *Resolver) RegisterInline(keyRef, plaintext string, expires time.Time) {
	if keyRef == "" || plaintext == "" {
		return
	}
	sh := r.shards[shardIndex(keyRef)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.entries[keyRef] = inlineEntry{plain: plaintext, expires: expires}
}

// RevokeInline drops an inline entry. Console DELETE on an inline
// credential row calls this; subsequent ResolveInline returns
// ErrSecretNotFound and the worker's profile is skipped cleanly.
func (r *Resolver) RevokeInline(keyRef string) {
	if keyRef == "" {
		return
	}
	sh := r.shards[shardIndex(keyRef)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	delete(sh.entries, keyRef)
}

// shardIndex takes FNV-1a over the key bytes modulo shard count.
// Stable across runs, low collision, cheap.
func shardIndex(s string) int {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return int(h % 32)
}

// WithLookupTimeout overrides the per-call SecretLookup deadline.
func WithLookupTimeout(d time.Duration) func(*resolverConfig) {
	return func(c *resolverConfig) { c.LookupTimeout = d }
}

// WithOrgProvider overrides the fallback org provider name.
func WithOrgProvider(p string) func(*resolverConfig) {
	return func(c *resolverConfig) { c.OrgProvider = p }
}

// Resolve implements SecretResolver. Maps the endpoint's KeySource
// to the right lookup; an error or empty result yields "" so the
// worker can short-circuit without leaking whether the missing key
// was a row miss or a permission miss.
func (r *Resolver) Resolve(t observability.Trace, ep EvalEndpoint) (string, error) {
	if r == nil || r.lookup == nil {
		return "", errors.New("resolver not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LookupTimeout)
	defer cancel()
	switch ep.KeySource {
	case KeySourceBuiltin, "":
		return "", nil
	case KeySourceOrg:
		prov := r.cfg.OrgProvider
		if ep.BaseURL != "" {
			// Cheap inference: profile just used the same provider name
			// as its base URL host. Keeps the data model slim while
			// letting the console drop "provider": "openai" without a
			// redundant field.
			prov = deriveProviderFromURL(ep.BaseURL)
		}
		s, err := r.lookup.Org(ctx, prov)
		if err != nil {
			r.log("resolver org lookup failed provider=%s err=%v", prov, err)
			return "", err
		}
		return s, nil
	case KeySourceUser:
		owner := ep.KeyRef // the console stores user-scoped keys as ref=user_id|provider
		if owner == "" && t.UserID != "" {
			owner = t.UserID
		}
		s, err := r.lookup.User(ctx, deriveProviderFromURL(ep.BaseURL), owner)
		if err != nil {
			r.log("resolver user lookup failed user=%s err=%v", owner, err)
			return "", err
		}
		return s, nil
	case KeySourceInline:
		sh := r.shards[shardIndex(ep.KeyRef)]
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		entry, ok := sh.entries[ep.KeyRef]
		if !ok {
			return "", ErrSecretNotFound
		}
		if !entry.expires.IsZero() && time.Now().After(entry.expires) {
			return "", fmt.Errorf("inline secret expired at %s", entry.expires.Format(time.RFC3339))
		}
		return entry.plain, nil
	default:
		return "", fmt.Errorf("unknown key_source=%q", ep.KeySource)
	}
}

// deriveProviderFromURL pulls a slug-shaped provider name off the
// base URL host. Inside Nexus most endpoints align (e.g. "openai"
// → api.openai.com) so this is good enough as a default; consumers
// can override in EvalEndpoint if they need finer control.
func deriveProviderFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	// Strip scheme.
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Trim path.
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	// Take host[0] before the first dot.
	if i := strings.Index(s, "."); i >= 0 {
		return s[:i]
	}
	return s
}
