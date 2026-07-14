package core

// Canonical audit-log action names. We centralise them here so new admin
// actions cannot typo-bypass the audit trail by accident — callers reference
// these constants rather than string literals.
//
// Rule of thumb:
//   - everything is "<subject>.<verb>" so SQL filters like `WHERE action LIKE
//     'credential.%'` work even when we add more verbs later.
//   - The verb is past-tense lowercase so the row reads as a fact ("someone
//     did this"), not an instruction.
//
// v1.1 doc §2.2 listed these as the audit-action coverage bar; if a new
// state-changing action ships without registering here, treat that as a
// compliance gap and add the constant in the same PR.
const (
	// Subject: user account lifecycle (admin-scoped or member-self).
	AuditUserCreate = "user.create"
	AuditUserDelete = "user.delete"
	// Subject: password / OIDC login. Email/password login is the same data
	// line as SSO but kept distinct for filterability.
	AuditUserLogin = "user.login"  // password/email login, was "auth.login"
	AuditSSOLogin  = "sso.login"   // OIDC login
	AuditLogout    = "auth.logout" // session cookie cleared

	// Subject: virtual keys (per-org quota / RPM tokens).
	AuditVKeyCreate = "vkey.create"
	AuditVKeyRevoke = "vkey.revoke"

	// Subject: provider credentials (BYOK).
	AuditCredentialCreate = "credential.create"
	AuditCredentialRotate = "credential.rotate"
	AuditCredentialDelete = "credential.delete"

	// Subject: a user updating their own settings (e.g. enforce_limits).
	AuditMeUpdate = "me.update"
)

// AllAuditActions is the completeness bar a regression test compares against.
// Adding a new state-changing admin action must come with a constant above
// AND an entry here, otherwise the coverage test fails.
var AllAuditActions = []string{
	AuditUserCreate,
	AuditUserDelete,
	AuditUserLogin,
	AuditSSOLogin,
	AuditLogout,
	AuditVKeyCreate,
	AuditVKeyRevoke,
	AuditCredentialCreate,
	AuditCredentialRotate,
	AuditCredentialDelete,
	AuditMeUpdate,
}
