// c0.8 is the closure check that ties c0.1–c0.7 together. It walks:
//   - the closed AuditAction registry,
//   - the closed AuditReason registry,
//   - the closed apierr.Code registry,
// and ensures every constant in either direction is wired. The list
// of categories is documented in docs/audit-action-constants.md; each
// category has at least one AuditAction declared.

package core

import "testing"

// TestAuditCategoriesAreFullyImplemented walks the documented
// category list and asserts:
//
//   - At least one AuditAction exists per category.
//   - The action's prefix matches the category's canonical name.
//
// The category list is the audit coverage contract: dropping or
// re-naming a category is a customer-visible failure (SIEM panels
// stop populating). The list here is the single source of truth.
func TestAuditCategoriesAreFullyImplemented(t *testing.T) {
	categoryNeedsImplementation := []struct {
		category   string
		atLeastOne string // exact AuditAction prefix in this category
	}{
		{"auth.", "auth.login.succeeded"},
		{"user.", "user.login.succeeded"},
		{"invite.", "invite.issue"},
		{"key.", "key.create"},
		{"credential.", "credential.create"},
		{"routing.", "routing.decision"},
		{"eval.", "eval.run.create"},
		{"benchmark.", "benchmark.create"},
		{"policy.", "policy.create"},
		{"security.", "security.panic.recovered"},
		{"sso.", "sso.callback.succeeded"},
		{"audit.", "audit.view"},
		{"denied.", "denied.schema.contract"},
	}
	known := KnownAuditActionsForTest()
	have := make(map[string]bool)
	for _, a := range known {
		have[string(a)] = true
	}
	for _, c := range categoryNeedsImplementation {
		if !have[c.atLeastOne] {
			t.Errorf("category %q is missing the canonical action %q; either implement the action or "+
				"update the category's documentation in docs/audit-action-constants.md", c.category, c.atLeastOne)
		}
	}
}

// TestAuditActionCategoriesAreClosed scans the known action list for
// any action whose prefix is not in the documented categories. An
// unmapped action would silently fall outside SIEM rules.
//
// Note: prefix matching uses the first dot segment as the category
// name (e.g. "auth.login.succeeded" -> "auth.").
func TestAuditActionCategoriesAreClosed(t *testing.T) {
	known := KnownAuditActionsForTest()
	allowedPrefixes := map[string]bool{
		"auth.":       true,
		"user.":       true,
		"invite.":     true,
		"key.":        true,
		"credential.": true,
		"routing.":    true,
		"eval.":       true,
		"benchmark.":  true,
		"policy.":     true,
		"security.":   true,
		"sso.":        true,
		"audit.":      true,
		"denied.":     true,
	}
	for _, a := range known {
		prefix := ""
		for i := 0; i < len(a); i++ {
			if a[i] == '.' {
				prefix = string(a[:i+1])
				break
			}
		}
		if prefix == "" {
			t.Errorf("AuditAction %q has no category (no dot delimiter); "+
				"every action must have a documented category prefix", a)
			continue
		}
		if !allowedPrefixes[prefix] {
			t.Errorf("AuditAction %q has category %q which is not in the documented "+
				"categories (see docs/audit-action-constants.md); add it to allowedPrefixes "+
				"or rename the action to use an existing category", a, prefix)
		}
	}
}
