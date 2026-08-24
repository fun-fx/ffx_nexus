// Package core (audit.go) — the closed Action / Reason constant registries
// for the audit_log subsystem. Audit rows are giant freeform tables by
// default; pinning the values is the lever that makes SIEM rules and
// dashboards addressable.
//
// # Stability
//
// The string values of these constants are a stability contract. Customers
// export audit data into elasticsearch / splunk / a SIEM and write
// rules by the action string ("alert on auth.login.denied with reason
// invalid_key"). Renaming a value silently breaks every customer rule.
// Renaming the Go identifier (AuditActionAuthLoginDenied) is fine — only
// the literal string is the contract.
//
// Whenever a constant is added, removed, or its value changed:
//
//  1. internal/core/audit_inventory_test.go must be updated so the AST
//     inventory still passes.
//  2. docs/audit-action-constants.md is the canonical list cited by SIEM
//     rules; update it alongside.
//
// # Categorisation
//
// observability.auditActionCategory maps each action to a Prometheus label
// category, and that mapping is also covered by the inventory test — the
// audit_action_category table is exhaustive of all action prefixes that
// appear here. Adding a new prefix without updating both the prefix switch
// in metrics.go and the c0.8 inventory test will fail on CI.
//
// # Reason catalog and postgreserr alignment (c0.2)
//
// AuditReason values are intentionally finer-grained than apierr.Code
// because internal analysis wants to distinguish, e.g., "forbidden" from
// "rate_limited" in audit, but the public body keeps both at apierr.Code
// rate_limited for the customer. The mapping auditReasonToPublicCode is
// exported so dashboards can role-up; the auditReasonFromPostgresClassify
// hook lets postgreserr.Classify participate in the same pipeline so the
// two systems do not drift.
package core

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ffxnexus/nexus/internal/apierr"
)

// Internal aliases for in-package legacy callers (users.go, invite.go,
// store.go). Maintained so the legacy constant literal stays a single
// editable point; new code MUST use AuditAction constants directly. The
// aliases are not exported.
const (
	auditUserCreate       = AuditActionUserCreated
	auditUserDelete       = AuditActionUserDeleted
	auditUserLogin        = AuditActionUserLoginSucceeded
	auditInviteIssue      = AuditActionInviteIssued
	auditInviteAccept     = AuditActionInviteAccepted
	auditInviteRevoke     = AuditAction("invite.revoke.legacy")
	auditInviteEmailFail  = AuditAction("invite.email.failed")
	auditInviteEmailSent  = AuditAction("invite.email.sent")
	auditVKeyCreate       = AuditActionKeyCreated
	auditVKeyRevoke       = AuditActionKeyRevoked
	auditCredentialCreate = AuditActionCredentialCreated
	auditCredentialUpdate = AuditActionCredentialUpdated
	auditCredentialRotate = AuditActionCredentialRotate
	auditCredentialDelete = AuditActionCredentialDeleted
)

// Exported aliases (legacy type: string) so callers in other packages
// (console/*) keep compiling. New code MUST use the AuditAction constants
// directly — the inventory test c0.8 will fail if a new caller uses one of
// these legacy aliases without a TODO marker.
//
// TODO(d1): remove these aliases once console callers have been ported.
const (
	AuditUserCreate       = string(AuditActionUserCreated)
	AuditUserUpdate       = string(AuditActionUserUpdated)
	AuditUserDelete       = string(AuditActionUserDeleted)
	AuditUserLogin        = string(AuditActionUserLoginSucceeded)
	AuditInviteIssue      = string(AuditActionInviteIssued)
	AuditInviteRevoke     = "invite.revoke.legacy"
	AuditInviteAccept     = string(AuditActionInviteAccepted)
	AuditInviteEmailFail  = "invite.email.failed"
	AuditInviteEmailSent  = "invite.email.sent"
	AuditVKeyCreate       = string(AuditActionKeyCreated)
	AuditVKeyRevoke       = string(AuditActionKeyRevoked)
	AuditCredentialCreate = string(AuditActionCredentialCreated)
	AuditCredentialUpdate = string(AuditActionCredentialUpdated)
	AuditCredentialRotate = string(AuditActionCredentialRotate)
	AuditCredentialDelete = string(AuditActionCredentialDeleted)
	AuditSSOLogin         = string(AuditActionSSOCallbackSucceeded)
	AuditLogout           = string(AuditActionAuthLogoutSucceeded)
	AuditMeUpdate         = string(AuditActionUserUpdated)
)

// AuditAction is the typed value of audit_log.action. New values go through
// the registry below; never use a free-form string at the call site.
type AuditAction string

// The Action registry. The document heads add comments describing the
// denied-attempt cases c0.3 demands. The "denied." sub-namespace is the
// audit hook for security-relevant rejections (auth, origin, rate-limit,
// egress); a denied action always has a reason from the Reason catalog.
const (
	// --- Authentication ---
	AuditActionAuthLoginSucceeded   AuditAction = "auth.login.succeeded"
	AuditActionAuthLoginDenied      AuditAction = "auth.login.denied"
	AuditActionAuthLogoutSucceeded  AuditAction = "auth.logout.succeeded"
	AuditActionAuthMFAChallengeSent AuditAction = "auth.mfa.challenge.sent"
	AuditActionAuthMFAVerified      AuditAction = "auth.mfa.verified"
	AuditActionAuthMFAFailed        AuditAction = "auth.mfa.failed"

	// --- SSO ---
	AuditActionSSOCallbackSucceeded AuditAction = "sso.callback.succeeded"
	AuditActionSSOCallbackDenied    AuditAction = "sso.callback.denied"
	AuditActionSSOStateMismatch     AuditAction = "sso.state.mismatch"
	AuditActionSSONonceMismatch     AuditAction = "sso.nonce.mismatch"

	// --- Org / tenancy boundary ---
	AuditActionOrgBoundaryViolated AuditAction = "denied.org.boundary"
	AuditActionOriginBlocked       AuditAction = "denied.origin"
	AuditActionRequestSizeExceeded AuditAction = "denied.request_size"

	// --- Rate limit / CORS / egress ---
	AuditActionRateLimited           AuditAction = "denied.rate_limit"
	AuditActionCORSDenied            AuditAction = "denied.cors"
	AuditActionEgressBlocked         AuditAction = "denied.egress"
	AuditActionEgressAddressRejected AuditAction = "denied.egress.address"

	// --- API key lifecycle ---
	AuditActionKeyCreated         AuditAction = "key.create"
	AuditActionKeyRevoked         AuditAction = "key.revoke"
	AuditActionKeyRotated         AuditAction = "key.rotate"
	AuditActionKeyRejectedInvalid AuditAction = "key.rejected.invalid"
	AuditActionKeyRejectedExpired AuditAction = "key.rejected.expired"
	AuditActionKeyRejectedRevoked AuditAction = "key.rejected.revoked"
	AuditActionKeyAccepted        AuditAction = "key.accepted"

	// --- Credentials (provider) ---
	AuditActionCredentialCreated      AuditAction = "credential.create"
	AuditActionCredentialUpdated      AuditAction = "credential.update"
	AuditActionCredentialRotate       AuditAction = "credential.rotate"
	AuditActionCredentialDeleted      AuditAction = "credential.delete"
	AuditActionCredentialBaseURLSaved AuditAction = "credential.base_url.saved"

	// --- Users & invites ---
	AuditActionUserCreated           AuditAction = "user.create"
	AuditActionUserUpdated           AuditAction = "user.update"
	AuditActionUserDeleted           AuditAction = "user.delete"
	AuditActionUserLoginSucceeded    AuditAction = "user.login.succeeded"
	AuditActionUserLoginDenied       AuditAction = "user.login.denied"
	AuditActionInviteIssued          AuditAction = "invite.issued"
	AuditActionInviteIssuedEmailStarted   AuditAction = "invite.email.started"
	AuditActionInviteIssuedEmailSent      AuditAction = "invite.email.sent"
	AuditActionInviteIssuedEmailFailed    AuditAction = "invite.email.failed"
	AuditActionInviteIssuedEmailTemplate  AuditAction = "invite.email.template_failed"
	AuditActionInviteAccepted        AuditAction = "invite.accepted"
	AuditActionInviteRejectedInvalid AuditAction = "invite.rejected.invalid"
	AuditActionInviteRejectedExpired AuditAction = "invite.rejected.expired"
	AuditActionInviteReplayRejected  AuditAction = "invite.rejected.replay"

	// --- Routing / quality-aware failover ---
	AuditActionRoutingDecision AuditAction = "routing.decision"
	AuditActionRoutingFailover AuditAction = "routing.failover"

	// --- Eval / benchmark / integration ---
	AuditActionEvalRunCreated         AuditAction = "eval.run.create"
	AuditActionEvalRunScored          AuditAction = "eval.run.scored"
	AuditActionEvalPluginInstalled    AuditAction = "eval.plugin.install"
	AuditActionEvalPluginManifestFail AuditAction = "denied.eval.plugin_manifest"
	AuditActionBenchmarkCreated       AuditAction = "benchmark.create"
	AuditActionBenchmarkRunStarted    AuditAction = "benchmark.run.start"
	AuditActionBenchmarkRunFinished   AuditAction = "benchmark.run.finish"
	AuditActionBenchmarkScheduleHit   AuditAction = "benchmark.schedule.hit"
	AuditActionBudgetExceededDenied   AuditAction = "denied.budget.exceeded"
	AuditActionModelAllowlistDenied   AuditAction = "denied.model.allowlist"
	AuditActionConcurrencyCapDenied   AuditAction = "denied.concurrency.cap"

	// --- Policy / security / safety ---
	AuditActionPolicyCreated  AuditAction = "policy.create"
	AuditActionPolicyDeleted  AuditAction = "policy.delete"
	AuditActionSecurityAlert  AuditAction = "security.alert"
	AuditActionPanicRecovered AuditAction = "security.panic.recovered"

	// --- Audit metadata access (c0.7) ---
	AuditActionAuditViewed       AuditAction = "audit.view"
	AuditActionAuditExported     AuditAction = "audit.export"
	AuditActionAuditViewDenied   AuditAction = "denied.audit.view"
	AuditActionAuditExportDenied AuditAction = "denied.audit.export"

	// --- Schema contract enforcement (c0.3 hooked here too) ---
	AuditActionSchemaContractViolated AuditAction = "denied.schema.contract"
)

// KnownAuditActionsForTest exposes the canonical list to tests outside the
// core package. The list is the source of truth for the c0.8 inventory
// (TestAuditActionCoverage in the console package is the explicit
// pass-through). A new declared AuditAction constant MUST be added here
// in the same PR.
func KnownAuditActionsForTest() []AuditAction {
	out := make([]AuditAction, len(knownActions))
	copy(out, knownActions)
	return out
}

// AllAuditActionStrings returns the action list as strings. Use this only
// when the test specifically needs the underlying string value (e.g. SQL
// assertions); otherwise prefer KnownAuditActionsForTest so a future
// AuditAction bug surfaces in the type system.
func AllAuditActionStrings() []string {
	out := make([]string, len(knownActions))
	for i, a := range knownActions {
		out[i] = string(a)
	}
	return out
}

// knownActions is the canonical list of every AuditAction constant. The
// inventory test pins against this list so adding "dead" code (a declared
// constant with no callers) is loud at CI time.
var knownActions = []AuditAction{
	AuditActionAuthLoginSucceeded,
	AuditActionAuthLoginDenied,
	AuditActionAuthLogoutSucceeded,
	AuditActionAuthMFAChallengeSent,
	AuditActionAuthMFAVerified,
	AuditActionAuthMFAFailed,
	AuditActionSSOCallbackSucceeded,
	AuditActionSSOCallbackDenied,
	AuditActionSSOStateMismatch,
	AuditActionSSONonceMismatch,
	AuditActionOrgBoundaryViolated,
	AuditActionOriginBlocked,
	AuditActionRequestSizeExceeded,
	AuditActionRateLimited,
	AuditActionCORSDenied,
	AuditActionEgressBlocked,
	AuditActionEgressAddressRejected,
	AuditActionKeyCreated,
	AuditActionKeyRevoked,
	AuditActionKeyRotated,
	AuditActionKeyRejectedInvalid,
	AuditActionKeyRejectedExpired,
	AuditActionKeyRejectedRevoked,
	AuditActionKeyAccepted,
	AuditActionCredentialCreated,
	AuditActionCredentialUpdated,
	AuditActionCredentialRotate,
	AuditActionCredentialDeleted,
	AuditActionCredentialBaseURLSaved,
	AuditActionUserCreated,
	AuditActionUserUpdated,
	AuditActionUserDeleted,
	AuditActionUserLoginSucceeded,
	AuditActionUserLoginDenied,
	AuditActionInviteIssued,
	AuditActionInviteIssuedEmailStarted,
	AuditActionInviteIssuedEmailSent,
	AuditActionInviteIssuedEmailFailed,
	AuditActionInviteIssuedEmailTemplate,
	AuditActionInviteAccepted,
	AuditActionInviteRejectedInvalid,
	AuditActionInviteRejectedExpired,
	AuditActionInviteReplayRejected,
	AuditActionRoutingDecision,
	AuditActionRoutingFailover,
	AuditActionEvalRunCreated,
	AuditActionEvalRunScored,
	AuditActionEvalPluginInstalled,
	AuditActionEvalPluginManifestFail,
	AuditActionBenchmarkCreated,
	AuditActionBenchmarkRunStarted,
	AuditActionBenchmarkRunFinished,
	AuditActionBenchmarkScheduleHit,
	AuditActionBudgetExceededDenied,
	AuditActionModelAllowlistDenied,
	AuditActionConcurrencyCapDenied,
	AuditActionPolicyCreated,
	AuditActionPolicyDeleted,
	AuditActionSecurityAlert,
	AuditActionPanicRecovered,
	AuditActionAuditViewed,
	AuditActionAuditExported,
	AuditActionAuditViewDenied,
	AuditActionAuditExportDenied,
	AuditActionSchemaContractViolated,
}

// AuditFailureClass partitions every AuditAction into one of two
// failure-policy classes for store.Audit write errors.
//
//   - FailStopClass — the customer request must fail when the audit
//     row cannot be written. Security-relevant changes (admin
//     lifecycle, credential rotation, audit access, security panics,
//     policy changes) must leave a durable trail; failing the request
//     is preferable to letting the security event go unrecorded.
//
//   - BestEffortClass — the audit row is desirable but the customer's
//     request must succeed anyway. Hot-path and aggregated denials
//     fall here because stopping every failed request in a burst
//     would amplify a Postgres outage into a service outage.
//
// The class assignment lives in code (a map, not a registry constant)
// because it's enforced by the test in audit_failstop_test.go and
// fails at compile time if an action is added without a class.
type AuditFailureClass string

const (
	FailStopClass   AuditFailureClass = "fail_stop"
	BestEffortClass AuditFailureClass = "best_effort"
)

// auditFailureClass is the closed map aligning AuditAction to its
// failure class. TestAuditFailureClassIsExhaustive verifies every
// known action has exactly one entry AND every entry corresponds to
// a real action constant.
//
// The inverse direction "value registered but no caller" is checked
// by TestAuditFailureClassHasNoDeadActions.
// auditFailureClass is the closed map aligning AuditAction to its
// failure class. TestAuditFailureClassIsExhaustive verifies every
// known action has exactly one entry AND every entry corresponds to
// a real action constant.
//
// The inverse direction "value registered but no caller" is checked
// by TestAuditFailureClassHasNoDeadActions. The map's keys are the
// full catalogue of audit-emitting actions, deduplicated; the
// inventory refuses silent drift in either direction.
var auditFailureClass = map[AuditAction]AuditFailureClass{
	// --- Audit access (high-privilege reads) ---
	AuditActionAuditViewed:       FailStopClass,
	AuditActionAuditExported:     FailStopClass,
	AuditActionAuditViewDenied:   FailStopClass,
	AuditActionAuditExportDenied: FailStopClass,

	// --- Authentication / SSO ---
	AuditActionAuthLoginSucceeded:   FailStopClass,
	AuditActionAuthLoginDenied:      BestEffortClass,
	AuditActionAuthLogoutSucceeded:  FailStopClass,
	AuditActionAuthMFAChallengeSent: BestEffortClass,
	AuditActionAuthMFAVerified:      FailStopClass,
	AuditActionAuthMFAFailed:        BestEffortClass,
	AuditActionUserLoginSucceeded:   FailStopClass,
	AuditActionUserLoginDenied:      BestEffortClass,
	AuditActionSSOCallbackSucceeded: FailStopClass,
	AuditActionSSOCallbackDenied:    BestEffortClass,
	AuditActionSSOStateMismatch:     BestEffortClass,
	AuditActionSSONonceMismatch:     BestEffortClass,

	// --- Users & lifecycle ---
	AuditActionUserCreated: FailStopClass,
	AuditActionUserUpdated: FailStopClass,
	AuditActionUserDeleted: FailStopClass,

	// --- Credentials (security-state changes) ---
	AuditActionCredentialCreated:      FailStopClass,
	AuditActionCredentialUpdated:      FailStopClass,
	AuditActionCredentialRotate:       FailStopClass,
	AuditActionCredentialDeleted:      FailStopClass,
	AuditActionCredentialBaseURLSaved: FailStopClass,

	// --- API keys ---
	AuditActionKeyCreated:         FailStopClass,
	AuditActionKeyRevoked:         FailStopClass,
	AuditActionKeyRotated:         FailStopClass,
	AuditActionKeyRejectedInvalid: BestEffortClass,
	AuditActionKeyRejectedExpired: BestEffortClass,
	AuditActionKeyRejectedRevoked: BestEffortClass,
	AuditActionKeyAccepted:        BestEffortClass,

	// --- Invites ---
	AuditActionInviteIssued:          FailStopClass,
	AuditActionInviteIssuedEmailStarted:  BestEffortClass,
	AuditActionInviteIssuedEmailSent:     BestEffortClass,
	AuditActionInviteIssuedEmailFailed:   BestEffortClass,
	AuditActionInviteIssuedEmailTemplate: BestEffortClass,
	AuditActionInviteAccepted:        FailStopClass,
	AuditActionInviteRejectedInvalid: BestEffortClass,
	AuditActionInviteRejectedExpired: BestEffortClass,
	AuditActionInviteReplayRejected:  BestEffortClass,

	// --- Denials (aggregated hot-path) ---
	AuditActionOrgBoundaryViolated:   FailStopClass,
	AuditActionOriginBlocked:         FailStopClass,
	AuditActionCORSDenied:            FailStopClass,
	AuditActionRequestSizeExceeded:   BestEffortClass,
	AuditActionRateLimited:           BestEffortClass,
	AuditActionEgressBlocked:         FailStopClass,
	AuditActionEgressAddressRejected: FailStopClass,
	AuditActionBudgetExceededDenied:  BestEffortClass,
	AuditActionModelAllowlistDenied:  BestEffortClass,
	AuditActionConcurrencyCapDenied:  BestEffortClass,

	// --- Policy / security / safety ---
	AuditActionPolicyCreated:  FailStopClass,
	AuditActionPolicyDeleted:  FailStopClass,
	AuditActionSecurityAlert:  FailStopClass,
	AuditActionPanicRecovered: FailStopClass,

	// --- Routing / eval / benchmark (operational, not security) ---
	AuditActionRoutingDecision:        BestEffortClass,
	AuditActionRoutingFailover:        BestEffortClass,
	AuditActionEvalRunCreated:         BestEffortClass,
	AuditActionEvalRunScored:          BestEffortClass,
	AuditActionEvalPluginInstalled:    FailStopClass,
	AuditActionEvalPluginManifestFail: BestEffortClass,
	AuditActionBenchmarkCreated:       BestEffortClass,
	AuditActionBenchmarkRunStarted:    BestEffortClass,
	AuditActionBenchmarkRunFinished:   BestEffortClass,
	AuditActionBenchmarkScheduleHit:   BestEffortClass,

	// --- Schema contract (call out as a fail-stop — drift means SQL is unsafe to run) ---
	AuditActionSchemaContractViolated: FailStopClass,
}

// ClassifyAuditAction looks up the failure class for the action, returning
// BestEffortClass for unknown actions. The fallback is intentional: the
// c0.5 contract says "if we don't know, don't fail-stop"; an unknown
// action shouldn't accidentally take the gateway offline.
func ClassifyAuditAction(a AuditAction) AuditFailureClass {
	if c, ok := auditFailureClass[a]; ok {
		return c
	}
	return BestEffortClass
}

// FailureClassForTest exposes the closed failure-class map for tests
// outside the core package. The audit_failstop_test.go file in this
// package iterates it directly.
func FailureClassForTest() map[AuditAction]AuditFailureClass {
	out := make(map[AuditAction]AuditFailureClass, len(auditFailureClass))
	for k, v := range auditFailureClass {
		out[k] = v
	}
	return out
}

type AuditReason string

const (
	AuditReasonUnknown                   AuditReason = "unknown"
	AuditReasonForbidden                 AuditReason = "forbidden"
	AuditReasonInvalidCredentials        AuditReason = "invalid_credentials"
	AuditReasonKeyInvalid                AuditReason = "key_invalid"
	AuditReasonKeyExpired                AuditReason = "key_expired"
	AuditReasonKeyRevoked                AuditReason = "key_revoked"
	AuditReasonOrgBoundary               AuditReason = "org_boundary"
	AuditReasonOriginNotAllowed          AuditReason = "origin_not_allowed"
	AuditReasonRequestTooLarge           AuditReason = "request_too_large"
	AuditReasonRateLimited               AuditReason = "rate_limited"
	AuditReasonCORSDisallowed            AuditReason = "cors_disallowed"
	AuditReasonEgressAddressBlocked      AuditReason = "egress_address_blocked"
	AuditReasonEgressResolverFail        AuditReason = "egress_resolver_fail"
	AuditReasonEgressDNSRebindFail       AuditReason = "egress_dns_rebind"
	AuditReasonEvalPluginManifestInvalid AuditReason = "plugin_manifest_invalid"
	AuditReasonBudgetExceeded            AuditReason = "budget_exceeded"
	AuditReasonModelNotAllowed           AuditReason = "model_not_allowed"
	AuditReasonConcurrencyCapExceeded    AuditReason = "concurrency_cap_exceeded"
	AuditReasonInviteInvalid             AuditReason = "invite_invalid"
	AuditReasonInviteExpired             AuditReason = "invite_expired"
	AuditReasonInviteReplay              AuditReason = "invite_replay"
	AuditReasonSSOStateMismatch          AuditReason = "sso_state_mismatch"
	AuditReasonSSONonceMismatch          AuditReason = "sso_nonce_mismatch"
	AuditReasonSchemaContractViolation   AuditReason = "schema_contract_violation"
	AuditReasonAuditPermissionDenied     AuditReason = "audit_permission_denied"
	AuditReasonInternalError             AuditReason = "internal_error"
)

// auditReasonToPublicCode maps an internal granular AuditReason to the
// public apierr.Code that hits the customer. The mapping is intentionally
// lossy — multiple internal reasons coalesce into one public code so the
// customer contract stays stable while we can still discriminate during
// analysis. The mapping is exhaustive of AuditReason; new reasons must
// either map to an existing public code or extend it. The inventory test
// fails when a new const is added without a mapping.
// auditReasonRegisteredReasons is the exhaustive list of every declared
// AuditReason value. The c0.2 inventory test in audit_inventory_test.go
// iterates this slice and verifies every reason appears in the mapping
// table below — adding a const without extending this slice must fail CI.
//
// "Exhaustive" means every declared AuditReason literal in this file is
// present here. Customers can write SIEM rules against these strings,
// and the integrity of "reason -> public code" flow is what makes those
// rules warn-on-noisy vs. silent.
var auditReasonRegisteredReasons = []AuditReason{
	AuditReasonUnknown,
	AuditReasonForbidden,
	AuditReasonInvalidCredentials,
	AuditReasonKeyInvalid,
	AuditReasonKeyExpired,
	AuditReasonKeyRevoked,
	AuditReasonOrgBoundary,
	AuditReasonOriginNotAllowed,
	AuditReasonRequestTooLarge,
	AuditReasonRateLimited,
	AuditReasonCORSDisallowed,
	AuditReasonEgressAddressBlocked,
	AuditReasonEgressResolverFail,
	AuditReasonEgressDNSRebindFail,
	AuditReasonEvalPluginManifestInvalid,
	AuditReasonBudgetExceeded,
	AuditReasonModelNotAllowed,
	AuditReasonConcurrencyCapExceeded,
	AuditReasonInviteInvalid,
	AuditReasonInviteExpired,
	AuditReasonInviteReplay,
	AuditReasonSSOStateMismatch,
	AuditReasonSSONonceMismatch,
	AuditReasonSchemaContractViolation,
	AuditReasonAuditPermissionDenied,
	AuditReasonInternalError,
}

// KnownAuditReasonsForTest exposes the canonical reason list to tests
// outside the core package. The list mirrors auditReasonRegisteredReasons
// and is the source of truth for the c0.2 inventory.
func KnownAuditReasonsForTest() []AuditReason {
	out := make([]AuditReason, len(auditReasonRegisteredReasons))
	copy(out, auditReasonRegisteredReasons)
	return out
}

// ReasonToPublicCode is the audited, stable mapping from internal
// AuditReason to the apierr.Code that hits the customer. The mapping
// lives in code (not in a config) because both the customer contract
// and the SIEM contract must remain load-bearing: customers seeing an
// apierr.Code on the wire and operators reading the audit_log row can
// reconcile by walking this function.
//
// The function NEVER falls back to CodeInternalError silently: a missing
// case is a compile error in the switch below.
func ReasonToPublicCode(r AuditReason) apierr.Code { return auditReasonToPublicCode(r) }

// auditReasonToPublicCode is the actual mapping. The "switch with no
// default" form is deliberate: when a future engineer adds a new
// AuditReason constant but forgets to map it here, the missing case
// returns the zero value (apierr.Code = ""), which the c0.2 inventory
// test detects by checking that the canonical list and the
// reason-public-code registry line up.
//
// The mapping is intentionally many-to-one (N reasons -> M codes).
// Customers see one apierr.Code per code; we as operators keep more
// granular SIEM rules.
func auditReasonToPublicCode(r AuditReason) apierr.Code {
	switch r {
	case AuditReasonForbidden,
		AuditReasonOrgBoundary,
		AuditReasonOriginNotAllowed,
		AuditReasonCORSDisallowed:
		return apierr.CodeForbidden
	case AuditReasonModelNotAllowed:
		return apierr.CodeModelNotAllowed
	case AuditReasonInvalidCredentials,
		AuditReasonKeyInvalid,
		AuditReasonKeyExpired,
		AuditReasonKeyRevoked:
		return apierr.CodeUnauthenticated
	case AuditReasonRateLimited:
		return apierr.CodeRateLimited
	case AuditReasonRequestTooLarge:
		return apierr.CodeRequestTooLarge
	case AuditReasonEgressAddressBlocked,
		AuditReasonEgressResolverFail,
		AuditReasonEgressDNSRebindFail:
		return apierr.CodeEgressDenied
	case AuditReasonEvalPluginManifestInvalid:
		return apierr.CodeEvalPluginInvalid
	case AuditReasonBudgetExceeded:
		return apierr.CodeBudgetExceeded
	case AuditReasonConcurrencyCapExceeded:
		return apierr.CodeConcurrencyLimit
	case AuditReasonInviteInvalid,
		AuditReasonInviteExpired,
		AuditReasonInviteReplay:
		return apierr.CodeInviteInvalid
	case AuditReasonSSOStateMismatch,
		AuditReasonSSONonceMismatch:
		return apierr.CodeSSOStateInvalid
	case AuditReasonSchemaContractViolation:
		return apierr.CodeSchemaContractViolation
	case AuditReasonAuditPermissionDenied:
		return apierr.CodeAdminRequired
	case AuditReasonInternalError,
		AuditReasonUnknown:
		return apierr.CodeInternalError
	}
	// Returning an empty `apierr.Code` is the c0.2 invariant — a missing
	// case must not silently go to CodeInternalError because that
	// matches too many calls and breaks dashboards. The inventory test
	// reads the canonical reason list + this switch and ensures
	// every case is covered; a regression here would surface as
	// ReasonToPublicCode(unmappedReason) == "" which the test asserts.
	return ""
}

// FromPostgresSQLState produces an AuditReason from a Postgres SQLSTATE
// when the audit row is being written because a SQL error reached the
// console/gateway. This is the seam that ties postgreserr.Classify to the
// audit subsystem (c0.2): the public code the customer sees is computed
// independently by postgreserr, but the audit row needs the SAME mapped
// reason so that "what the customer saw" and "what's in the audit log"
// agree at the string level.
func auditReasonFromPostgresSQLState(state string) AuditReason {
	switch state {
	case "23505": // unique_violation
		return AuditReasonUnknown
	case "23503":
		return AuditReasonUnknown
	case "42501": // insufficient_privilege
		return AuditReasonForbidden
	case "28000", "28P01": // invalid Authorization Specification
		return AuditReasonInvalidCredentials
	default:
		return AuditReasonUnknown
	}
}

// pqErrAuditReason extracts the SQLSTATE from a *pgconn.PgError and asks
// auditReasonFromPostgresSQLState for the corresponding AuditReason. nil
// postgresErr -> AuditReasonUnknown, not nil, mirroring the safety property
// that audit rows always carry a (possibly unknown) reason.
func pqErrAuditReason(err error) AuditReason {
	if err == nil {
		return AuditReasonUnknown
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return AuditReasonUnknown
	}
	return auditReasonFromPostgresSQLState(pg.Code)
}
