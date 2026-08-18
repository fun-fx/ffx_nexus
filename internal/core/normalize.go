package core

import "strings"

// normaliseEmail lowercases + trims an email so invite rows compare the
// same way the users unique constraint does (the schema also stores the
// raw string as supplied; the lookup paths still match because callers
// are funnelled through this helper before either table).
func normaliseEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
