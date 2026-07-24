package evals

import (
	"errors"
	"strings"
	"time"
)

// KeySource identifies where a profile's judge/embeddings API key comes
// from. Same enum will be reused on the request side of `/api/me/eval/
// profiles` and from the Go→Python sidecar override later.
//
//   - org      — pulled from the org-shared provider_credentials row
//     (user_id IS NULL). Visible only to admins and the server.
//   - user     — pulled from the BYOK provider_credentials row owned by
//     the requesting user. Surfaced back as `key_id` for the
//     client so it can be renamed without leaking the secret.
//   - inline   — supplied directly in the request body (an admin-only
//     feature; encrypted at rest in eval_credentials).
//   - builtin  — for heuristic profiles (PII / Completeness) which
//     never call an external model. Used as the source of
//     truth for the `kind` discriminator.
type KeySource string

const (
	KeySourceOrg     KeySource = "org"
	KeySourceUser    KeySource = "user"
	KeySourceInline  KeySource = "inline"
	KeySourceBuiltin KeySource = "builtin"
)

// ProfileKind is the discriminator for the Evaluator factory. New kinds
// register themselves in `evaluatorRegistry` (see profile_registry.go
// in PR #137). Keeping the enum small here means adding a new metric
// is just: 1) implement Evaluator, 2) Register(builder), 3) UI.
type ProfileKind string

const (
	ProfileHeuristicPII          ProfileKind = "heuristic_pii"
	ProfileHeuristicCompleteness ProfileKind = "heuristic_completeness"
	ProfileSLMJudge              ProfileKind = "slm_judge"
	ProfileRemoteEval            ProfileKind = "remote_eval"
)

// Scope identifies visibility. Mirrors gateway.Scope so the model-side
// ergonomics are identical (PR #132/#133/#134). Today the console is
// single-tenant ("default" org) so `org` collapses to "visible to every
// member"; the wiring is identical to gateway.Scope so when we ship
// multi-tenancy the surface does not change.
type Scope string

const (
	ScopeOrg  Scope = "org"
	ScopeUser Scope = "user"
)

// EvalEndpoint is the per-profile connection info. Reuses the model-side
// fields (base_url, model) but adds a key source discriminator so a
// profile can piggy-back on the operator's anonymised credentials
// without ever seeing the plaintext key back in the response.
type EvalEndpoint struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// KeySource is one of the KeySource* enum values. The secret NEVER
	// leaves the server through this struct.
	KeySource KeySource `json:"key_source"`
	// KeyRef is the row id into (org_id, provider_credentials.id). For
	// `inline` keys it is a stable opaque token the client treats as a
	// surrogate (and the server replaces with the real secret at
	// evaluation time).
	KeyRef string `json:"key_ref,omitempty"`
}

// DefaultProfileSampleRate is the bake-in value when a profile omits
// the field. Aligns with the historical NEXUS_EVAL_SAMPLE_RATE=1.0.
const DefaultProfileSampleRate = 1.0

// MaxSampleRate is the upper bound for a per-profile sample_rate. Equal
// to 1.0 (every trace). Anything higher is clamped on Persist().
const MaxSampleRate = 1.0

// EvalProfile is the per-eval config the user persists. The struct is
// JSON-serialisable so the console's GET/PATCH endpoints can round-trip
// it directly without a translation layer.
//
// Validation is performed by Validate() (returns an error) rather than
// panicking in constructors — call sites use the PATCH path and
// shouldn't crash on a stray bad request.
type EvalProfile struct {
	// ID — server-assigned when the row is persisted; empty on create.
	ID string `json:"id"`
	// Name — display-only label surfaced in UI and audit log. Required.
	Name string `json:"name"`
	// Kind is the discriminator that picks the evaluator factory.
	Kind ProfileKind `json:"kind"`
	// Scope declares visibility. See the godoc on `Scope`.
	Scope Scope `json:"scope"`
	// OwnerUserID is set when Scope == ScopeUser. Empty for org-scope
	// profiles. Mapped at GET-time to the caller.
	OwnerUserID string `json:"owner_user_id,omitempty"`
	// Endpoint is the connection info. For builtin heuristics the base
	// URL and model are ignored; KeySource must be KeySourceBuiltin.
	Endpoint EvalEndpoint `json:"endpoint"`
	// Metrics — for `remote_eval` profiles, the active metric ids.
	// Ignored for non-remote kinds.
	Metrics []string `json:"metrics,omitempty"`
	// Threshold — pass threshold for `slm_judge` / `remote_eval`
	// evaluations; ignored for heuristics (their pass logic is
	// fixed in code).
	Threshold float64 `json:"threshold,omitempty"`
	// SampleRate in [0, 1]. 0 disables the profile (still persisted,
	// shown in UI as "off").
	SampleRate float64 `json:"sample_rate"`
	// Enabled is the master "off" switch. When false the profile is
	// not registered with the worker regardless of SampleRate.
	Enabled bool `json:"enabled"`
	// CreatedAt / UpdatedAt are populated server-side. Renderers
	// surface them in audit details panels; the UI does not edit them.
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// ProfilePatch is the incremental-update shape. All fields optional so
// the client can PATCH a single field without resending the whole row.
type ProfilePatch struct {
	Name       *string       `json:"name,omitempty"`
	Kind       *ProfileKind  `json:"kind,omitempty"`
	Scope      *Scope        `json:"scope,omitempty"`
	OwnerUser  *string       `json:"owner_user_id,omitempty"`
	Endpoint   *EvalEndpoint `json:"endpoint,omitempty"`
	Metrics    *[]string     `json:"metrics,omitempty"`
	Threshold  *float64      `json:"threshold,omitempty"`
	SampleRate *float64      `json:"sample_rate,omitempty"`
	Enabled    *bool         `json:"enabled,omitempty"`
}

// Validate reports structural issues. Returns the first error in a
// stable order so failing tests can pin against the message.
func (p *EvalProfile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("name is required")
	}
	if !validKind(p.Kind) {
		return errors.New("kind must be one of heuristic_pii | heuristic_completeness | slm_judge | remote_eval")
	}
	if p.Scope != ScopeOrg && p.Scope != ScopeUser {
		return errors.New("scope must be org or user")
	}
	if p.Scope == ScopeUser && strings.TrimSpace(p.OwnerUserID) == "" {
		return errors.New("scope=user requires owner_user_id")
	}
	if p.SampleRate < 0 || p.SampleRate > MaxSampleRate {
		return errors.New("sample_rate must be in [0,1]")
	}
	if p.Kind == ProfileHeuristicPII || p.Kind == ProfileHeuristicCompleteness {
		if p.Endpoint.KeySource != "" && p.Endpoint.KeySource != KeySourceBuiltin {
			return errors.New("heuristic profiles must use key_source=builtin")
		}
	} else {
		if p.Endpoint.BaseURL == "" {
			return errors.New("non-heuristic profiles require endpoint.base_url")
		}
		if p.Endpoint.KeySource == KeySourceInline && p.Endpoint.KeyRef == "" {
			return errors.New("key_source=inline requires a server-side key_ref token")
		}
	}
	return nil
}

// Clone returns a deep copy. Useful so the worker can hold a profile
// snapshot under w.mu while letting the runtime controller mutate the
// live struct via PATCH.
func (p *EvalProfile) Clone() *EvalProfile {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Metrics != nil {
		cp.Metrics = append([]string(nil), p.Metrics...)
	}
	cp.Endpoint = p.Endpoint
	return &cp
}

// validKind enumerates kinds the worker already understands. New kinds
// will be added as evaluator registrations; the registry pattern in
// PR #137 makes this an interface check rather than a string compare.
func validKind(k ProfileKind) bool {
	switch k {
	case ProfileHeuristicPII,
		ProfileHeuristicCompleteness,
		ProfileSLMJudge,
		ProfileRemoteEval:
		return true
	}
	return false
}
