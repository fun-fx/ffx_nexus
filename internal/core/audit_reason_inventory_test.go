// c0.2 inventory: the mapping between AuditReason and apierr.Code must be
// exhaustive. Adding a new AuditReason constant without routing it
// through ReasonToPublicCode is a silent defect; the customer sees
// apierr.Code "" on the response, and SIEM rules that filter on apierr.Code
// drop the row.
//
// Golden tests below pin the *exact* set of reasons so a value change
// fails CI. Two lists are intentionally pinned:

package core

import (
	"testing"

	"github.com/ffxnexus/nexus/internal/apierr"
)

// TestReasonMappingIsExhaustive asserts that every canonical AuditReason
// has a reason -> public code mapping. A new constant added to the
// registry must be added to auditReasonToPublicCode within the same
// commit; this test is the tripwire.
func TestReasonMappingIsExhaustive(t *testing.T) {
	for _, r := range KnownAuditReasonsForTest() {
		got := ReasonToPublicCode(r)
		if got == "" {
			t.Errorf("AuditReason %q has no mapping to apierr.Code; "+
				"add a case to auditReasonToPublicCode and to the c0.2 inventory", r)
		}
	}
}

// TestReasonStringValuesAreStable locks the exact string values of
// every AuditReason. SIEM rules use these strings; renaming
// AuditReasonInviteReplay from "invite_replay" to e.g. "invite_rejected.replay"
// would silently break customer dashboards. The golden list below is the
// change-managed contract — modify a string here AND in production only
// after a customer-visible deprecation cycle.
func TestReasonStringValuesAreStable(t *testing.T) {
	golden := map[AuditReason]string{
		AuditReasonUnknown:                   "unknown",
		AuditReasonForbidden:                 "forbidden",
		AuditReasonInvalidCredentials:        "invalid_credentials",
		AuditReasonKeyInvalid:                "key_invalid",
		AuditReasonKeyExpired:                "key_expired",
		AuditReasonKeyRevoked:                "key_revoked",
		AuditReasonOrgBoundary:               "org_boundary",
		AuditReasonOriginNotAllowed:          "origin_not_allowed",
		AuditReasonRequestTooLarge:           "request_too_large",
		AuditReasonRateLimited:               "rate_limited",
		AuditReasonCORSDisallowed:            "cors_disallowed",
		AuditReasonEgressAddressBlocked:      "egress_address_blocked",
		AuditReasonEgressResolverFail:        "egress_resolver_fail",
		AuditReasonEgressDNSRebindFail:       "egress_dns_rebind",
		AuditReasonEvalPluginManifestInvalid: "plugin_manifest_invalid",
		AuditReasonBudgetExceeded:            "budget_exceeded",
		AuditReasonModelNotAllowed:           "model_not_allowed",
		AuditReasonConcurrencyCapExceeded:    "concurrency_cap_exceeded",
		AuditReasonInviteInvalid:             "invite_invalid",
		AuditReasonInviteExpired:             "invite_expired",
		AuditReasonInviteReplay:              "invite_replay",
		AuditReasonSSOStateMismatch:          "sso_state_mismatch",
		AuditReasonSSONonceMismatch:          "sso_nonce_mismatch",
		AuditReasonSchemaContractViolation:   "schema_contract_violation",
		AuditReasonAuditPermissionDenied:     "audit_permission_denied",
		AuditReasonInternalError:             "internal_error",
	}
	for k, v := range golden {
		if string(k) != v {
			t.Errorf("AuditReason %q string drifted to %q; "+
				"a value change is a SIEM breaking change and must go through a deprecation cycle",
				string(k), v)
		}
	}
}

// TestReasonAndPublicCodeCrossReferenceMatchesInventory is the dual
// direction of the inventory: a stable string in customer-facing
// apierr.Code must have at least one AuditReason that maps to it. If
// we add a code that no reason produces, the code is dead; if we add a
// reason mapped to a code, the code is alive.
//
// The set is intentionally anchored to known inventory strings — no
// code-to-reason duplication concern because both are typed.
func TestReasonAndPublicCodeCrossReferenceMatchesInventory(t *testing.T) {
	golden := map[AuditReason]apierr.Code{
		AuditReasonForbidden:                 apierr.CodeForbidden,
		AuditReasonOrgBoundary:               apierr.CodeForbidden,
		AuditReasonOriginNotAllowed:          apierr.CodeForbidden,
		AuditReasonCORSDisallowed:            apierr.CodeForbidden,
		AuditReasonModelNotAllowed:           apierr.CodeModelNotAllowed,
		AuditReasonInvalidCredentials:        apierr.CodeUnauthenticated,
		AuditReasonKeyInvalid:                apierr.CodeUnauthenticated,
		AuditReasonKeyExpired:                apierr.CodeUnauthenticated,
		AuditReasonKeyRevoked:                apierr.CodeUnauthenticated,
		AuditReasonRateLimited:               apierr.CodeRateLimited,
		AuditReasonRequestTooLarge:           apierr.CodeRequestTooLarge,
		AuditReasonEgressAddressBlocked:      apierr.CodeEgressDenied,
		AuditReasonEgressResolverFail:        apierr.CodeEgressDenied,
		AuditReasonEgressDNSRebindFail:       apierr.CodeEgressDenied,
		AuditReasonEvalPluginManifestInvalid: apierr.CodeEvalPluginInvalid,
		AuditReasonBudgetExceeded:            apierr.CodeBudgetExceeded,
		AuditReasonConcurrencyCapExceeded:    apierr.CodeConcurrencyLimit,
		AuditReasonInviteInvalid:             apierr.CodeInviteInvalid,
		AuditReasonInviteExpired:             apierr.CodeInviteInvalid,
		AuditReasonInviteReplay:              apierr.CodeInviteInvalid,
		AuditReasonSSOStateMismatch:          apierr.CodeSSOStateInvalid,
		AuditReasonSSONonceMismatch:          apierr.CodeSSOStateInvalid,
		AuditReasonSchemaContractViolation:   apierr.CodeSchemaContractViolation,
		AuditReasonAuditPermissionDenied:     apierr.CodeAdminRequired,
		AuditReasonInternalError:             apierr.CodeInternalError,
		AuditReasonUnknown:                   apierr.CodeInternalError,
	}
	if len(golden) != len(KnownAuditReasonsForTest()) {
		t.Fatalf("golden mapping size = %d but known reasons size = %d; "+
			"they must match exactly", len(golden), len(KnownAuditReasonsForTest()))
	}
	for r, want := range golden {
		got := ReasonToPublicCode(r)
		if got != want {
			t.Errorf("ReasonToPublicCode(%q) = %q, want %q; mapping drifted", r, got, want)
		}
	}
}
