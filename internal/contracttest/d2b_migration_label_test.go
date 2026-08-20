// D-2b.23 migration label drift catcher.
//
// The chart's networkpolicy.yaml declares the
// migration Pod's role selector. The migration
// Job template MUST use the SAME label. If a
// future PR edits one without the other, the
// Pod never matches the policy and the integration
// gate finds nothing to enforce.
//
//go:build !integrationcni

package contracttest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationJobLabelMatchesNetworkPolicy(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Skip("cannot find project root")
	}
	npPath := filepath.Join(root, "deploy", "helm", "nexus", "templates", "networkpolicy.yaml")
	mjPath := filepath.Join(root, "deploy", "helm", "nexus", "templates", "migration-job.yaml")
	npBytes, err := os.ReadFile(npPath)
	if err != nil {
		// Skip path: the NetworkPolicy template and
		// the values.yaml `networkPolicy:` block are
		// not yet on the base branch. Same semantics
		// as TestFixtureLabelsConformToChart's skip:
		// make the chart-dependent test SKIP cleanly
		// so unrelated PRs do not get blocked.
		if os.IsNotExist(err) {
			t.Skip("networkpolicy.yaml template absent on disk — defer to D-2b chart-side PR")
		}
		t.Fatalf("read networkpolicy.yaml: %v", err)
	}
	mjBytes, err := os.ReadFile(mjPath)
	if err != nil {
		t.Fatalf("read migration-job.yaml: %v", err)
	}
	npText := string(npBytes)
	mjText := string(mjBytes)
	npMigration := extractMigrationSelector(npText)
	if npMigration == "" {
		t.Fatalf("could not extract migration selector label from networkpolicy.yaml")
	}
	if !strings.Contains(mjText, "app.kubernetes.io/component: "+npMigration) {
		t.Fatalf("migration-job.yaml uses different component value than networkpolicy.yaml: np=%s\nmj file:\n%s",
			npMigration, truncate(mjText))
	}
}

func extractMigrationSelector(np string) string {
	const marker = `$migrationSelector := dict`
	const component = `"app.kubernetes.io/component"`
	const sep = `" `
	const sepEnd = `"`
	idx := strings.Index(np, marker)
	if idx < 0 {
		return ""
	}
	rest := np[idx:]
	end := strings.Index(rest, sepEnd+" "+sepEnd)
	if end < 0 {
		return ""
	}
	chunk := rest
	comp := strings.Index(chunk, component)
	if comp < 0 {
		return ""
	}
	tail := chunk[comp+len(component)+2:]
	if strings.HasPrefix(tail, "migration") {
		return "migration"
	}
	return ""
}

func truncate(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

// moduleRoot returns the directory containing
// the closest go.mod ancestor. See the version
// in d2b_fixture_label_conformance_test.go — they
// used to be a private copy in this file and it
// had a walk bug when wd was ".".
func moduleRoot() (string, error) {
	return resolveModuleRoot()
}
