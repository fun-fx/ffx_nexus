package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
)

// Plugin names are unique per org, not per installation. Three admin routes —
// POST .../{name}/test, .../{name}/fire and .../{name}/automation — took that
// name straight from the URL and resolved it with the registry's org-blind
// Lookup / Enabled, which return the first or lowest-org match across every
// tenant.
//
// What that bought an attacker was not a listing leak. Each of those paths then
// uses the resolved plugin's *stored vendor credential*: test sends an
// authenticated probe, fire dispatches the plugin's buffered traces to its
// vendor, and automation creates a rule inside the vendor's workspace. So an
// admin of one org could spend another org's API key, push traffic into their
// vendor account, and read the vendor's reply.
//
// The registry already had the correct resolvers (LookupForOrg, EnabledForOrg,
// the latter carrying a comment saying it is "the only correct source for
// dispatch"). These tests pin that the admin paths use them.

func tenancyPluginYAML(t *testing.T, name string) string {
	t.Helper()
	return `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: ` + name + `
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 0.1
  collect:
    mode: webhook
    interval: 60s
  timeout: 30s
`
}

// registryWithOrgPlugins builds a registry holding one plugin per (org, name)
// pair given.
func registryWithOrgPlugins(t *testing.T, byOrg map[string]string) *evalplugin.Registry {
	t.Helper()
	reg := evalplugin.NewRegistry()
	var recs []evalplugin.Record
	for org, name := range byOrg {
		recs = append(recs, evalplugin.Record{
			Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase},
			OrgID:   org,
			Plugin:  evalPluginYAMLForTest(t, tenancyPluginYAML(t, name)),
			Enabled: true,
		})
	}
	if discarded := reg.Merge(recs); len(discarded) > 0 {
		t.Fatalf("registry discarded %d records the test needs", len(discarded))
	}
	return reg
}

// The probe must refuse a plugin belonging to another org, by name.
func TestPluginTesterRefusesAnotherOrgsPluginByName(t *testing.T) {
	reg := registryWithOrgPlugins(t, map[string]string{"org-a": "judge"})
	tester := newTester(reg, nil, nil)

	if _, ok := tester.resolve(context.Background(), "org-b", "judge"); ok {
		t.Error("org-b resolved org-a's plugin by name; the probe would authenticate " +
			"with org-a's stored vendor key and report the answer to org-b")
	}
	if _, ok := tester.resolve(context.Background(), "org-a", "judge"); !ok {
		t.Error("org-a can no longer reach its own plugin")
	}
}

// And by database id, which is the other accepted ref shape.
func TestPluginTesterRefusesAnotherOrgsPluginByID(t *testing.T) {
	reg := evalplugin.NewRegistry()
	store := orgAwareStubStore{rows: map[string]*evalplugin.PluginRecord{
		"row-a": {
			ID:       "row-a",
			OrgID:    "org-a",
			Name:     "judge",
			Enabled:  true,
			SpecYAML: tenancyPluginYAML(t, "judge"),
		},
	}}
	tester := newTester(reg, nil, nil).withSource(&pluginSourceAdapter{store: store})

	if _, ok := tester.resolve(context.Background(), "org-b", "row-a"); ok {
		t.Error("org-b resolved org-a's plugin by row id")
	}
	if _, ok := tester.resolve(context.Background(), "org-a", "row-a"); !ok {
		t.Error("org-a can no longer reach its own plugin by row id")
	}
}

// A cluster-wide plugin (org "") is one the operator installed through Helm and
// every tenant inherits it. Closing the cross-org hole must not break that, or
// operator-installed plugins would stop being testable.
func TestPluginTesterKeepsClusterWidePluginsReachable(t *testing.T) {
	reg := registryWithOrgPlugins(t, map[string]string{"": "helm-judge"})
	tester := newTester(reg, nil, nil)

	for _, org := range []string{"", "default", "org-a", "org-b"} {
		if _, ok := tester.resolve(context.Background(), org, "helm-judge"); !ok {
			t.Errorf("org %q cannot reach the operator's cluster-wide plugin", org)
		}
	}
}

// When two orgs each have a plugin under the same name, each must get its own —
// not whichever one sorts first. This is the case the old org-blind Lookup got
// silently wrong even for a caller who was fully entitled: it returned the
// lowest org id, so org-b pressing Test on *their* "judge" probed org-a's.
func TestPluginTesterPicksTheCallersOwnPluginOnANameCollision(t *testing.T) {
	reg := registryWithOrgPlugins(t, map[string]string{
		"org-a": "judge",
		"org-b": "judge",
	})
	tester := newTester(reg, nil, nil)

	for _, org := range []string{"org-a", "org-b"} {
		rec, ok := tester.resolve(context.Background(), org, "judge")
		if !ok {
			t.Fatalf("org %q could not resolve its own plugin", org)
		}
		if rec.OrgID != org {
			t.Errorf("org %q resolved to a record owned by %q", org, rec.OrgID)
		}
	}
}

// Firing is the highest-consequence path: it dispatches buffered traces to the
// resolved plugin's vendor. It must resolve within the caller's org only.
func TestManualFireResolvesWithinTheCallersOrg(t *testing.T) {
	reg := registryWithOrgPlugins(t, map[string]string{
		"org-a": "judge",
		"org-b": "other",
	})

	if p := pluginByNameForOrg(reg, "org-b", "judge"); p != nil {
		t.Error("org-b can fire org-a's plugin, pushing traces into org-a's vendor account")
	}
	if p := pluginByNameForOrg(reg, "org-a", "judge"); p == nil {
		t.Error("org-a can no longer fire its own plugin")
	}
	// Cluster-wide rows stay firable by everyone.
	wide := registryWithOrgPlugins(t, map[string]string{"": "helm-judge"})
	for _, org := range []string{"org-a", "default", ""} {
		if p := pluginByNameForOrg(wide, org, "helm-judge"); p == nil {
			t.Errorf("org %q cannot fire the operator's cluster-wide plugin", org)
		}
	}
}

// The legacy "default" placeholder and cluster-wide "" must resolve to the same
// tenant, or an operator's own console-created plugins stop matching their
// traffic — the bug evalplugin.NormalizeOrgID exists to prevent.
func TestFireTreatsLegacyDefaultOrgAsClusterWide(t *testing.T) {
	reg := registryWithOrgPlugins(t, map[string]string{"": "judge"})
	if p := pluginByNameForOrg(reg, evalplugin.LegacyDefaultOrgID, "judge"); p == nil {
		t.Errorf("a caller in the legacy %q org cannot reach a cluster-wide plugin",
			evalplugin.LegacyDefaultOrgID)
	}
}

// orgAwareStubStore is a PluginStore whose rows carry an org, so the id-keyed
// resolution path can be exercised across tenants.
type orgAwareStubStore struct {
	rows map[string]*evalplugin.PluginRecord
}

func (s orgAwareStubStore) List(_ context.Context, orgID string) ([]evalplugin.PluginRecord, error) {
	var out []evalplugin.PluginRecord
	for _, r := range s.rows {
		if orgID == "" || r.OrgID == orgID || r.OrgID == "" {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s orgAwareStubStore) Get(_ context.Context, id string) (*evalplugin.PluginRecord, error) {
	if r, ok := s.rows[id]; ok {
		return r, nil
	}
	// Fall back to name, matching how the real adapter resolves a ref.
	for _, r := range s.rows {
		if strings.EqualFold(r.Name, id) {
			return r, nil
		}
	}
	return nil, evalplugin.ErrPluginNotFound
}

func (s orgAwareStubStore) Save(_ context.Context, _ *evalplugin.PluginRecord) error { return nil }
func (s orgAwareStubStore) Delete(_ context.Context, _ string) error                 { return nil }
