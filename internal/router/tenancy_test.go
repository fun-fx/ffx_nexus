package router

import (
	"reflect"
	"strings"
	"testing"
)

// The router aggregates eval_scores across every org on purpose (see the godoc
// on ModelStats): it answers "is this model healthy", which is a question about
// a shared upstream rather than about anyone's data. That decision is only safe
// while the payload stays incapable of naming a tenant.
//
// This test is the enforcement. It is not decoration: /api/routing is reachable
// by any authenticated member, so the day someone adds a `SampleTraceID` or a
// `WorstUserID` field "for debugging", a cross-org aggregate becomes a cross-org
// disclosure — and nothing else in the codebase would notice, because the
// queries feeding it were always installation-wide and would keep passing.
func TestModelStatsExposesNoTenantData(t *testing.T) {
	// Substrings that would indicate a field can carry tenant identity or
	// user-authored content. Matched against the lower-cased field name.
	forbidden := []string{
		"org", "user", "email", "tenant",
		"trace", "span", "session", "turn",
		"prompt", "message", "content", "rationale", "input", "output",
		"key", "credential", "secret",
	}

	rt := reflect.TypeOf(ModelStats{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("ModelStats.%s looks tenant-identifying (matched %q).\n"+
					"The router's stats are aggregated across every org and are served to any\n"+
					"authenticated member, so this field would disclose one tenant's data to\n"+
					"another. Aggregate it per model, or build an org-keyed provider instead.",
					rt.Field(i).Name, bad)
			}
		}
	}
}

// A model id is not tenant data — it names a shared upstream, and two orgs
// calling gpt-4o are not learning anything about each other by both seeing that
// gpt-4o is degraded. This test states that so the rule above is not read as
// "no strings allowed", which would be the wrong lesson.
func TestModelStatsKeepsTheModelIdentifier(t *testing.T) {
	if _, ok := reflect.TypeOf(ModelStats{}).FieldByName("Model"); !ok {
		t.Fatal("ModelStats must key on the model; that is the whole aggregate")
	}
}
