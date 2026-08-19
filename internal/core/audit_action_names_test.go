// Drift sentinel: scripts/test_phase2.sh and any future test that
// reaches into audit_log by action name MUST agree with the typed
// constants in internal/core/audit.go.
//
// The audit-log cleanup in c0.x renamed "vkey.create"/"vkey.revoke" to
// the canonical "key." prefix. A tester who copy-pastes the old name
// into a SQL filter sees `count(*)` come back 0 — silent failure, no
// diff to look at. This file emits the canary: every action name that
// the scripts reference must match the constant. A drift lands in PR
// review, not in a redacted CI run log.
package core

import "testing"

// TestAuditActionNamesMatchScriptReferences checks that the strings
// the E2E suite uses to query audit_log are the same strings the
// runtime emits. The list is generated from a one-time grep over
// scripts/ at PR-merge time; bumping it requires rebumping the
// auditor's expectations, which is the desired friction.
func TestAuditActionNamesMatchScriptReferences(t *testing.T) {
	want := map[string]string{
		"key.create":             string(AuditActionKeyCreated),
		"key.revoke":             string(AuditActionKeyRevoked),
		"credential.create":      string(AuditActionCredentialCreated),
		"credential.update":      string(AuditActionCredentialUpdated),
		"credential.rotate":      string(AuditActionCredentialRotate),
		"credential.delete":      string(AuditActionCredentialDeleted),
		"invite.issued":          string(AuditActionInviteIssued),
		"invite.accepted":        string(AuditActionInviteAccepted),
		"audit.export":           string(AuditActionAuditExported),
		"audit.view":             string(AuditActionAuditViewed),
		"policy.create":          string(AuditActionPolicyCreated),
		"policy.delete":          string(AuditActionPolicyDeleted),
		"security.alert":         string(AuditActionSecurityAlert),
		"denied.egress":          string(AuditActionEgressBlocked),
		"denied.budget.exceeded": string(AuditActionBudgetExceededDenied),
		"denied.model.allowlist": string(AuditActionModelAllowlistDenied),
		"denied.concurrency.cap": string(AuditActionConcurrencyCapDenied),
	}
	for name, wantStr := range want {
		if AuditAction(name) != AuditAction(wantStr) {
			t.Errorf("audit action %q disagrees with canonical constant %q; "+
				"scripts that query audit_log with the literal will see zero rows. "+
				"Either update the constant or update the script.",
				name, wantStr)
		}
	}
}
