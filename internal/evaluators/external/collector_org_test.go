package external

import (
	"bytes"
	"context"
	"testing"

	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
)

// The collect side of a plugin is the one score-write path with no request
// context to inherit a tenant from. Before these tests, every score an external
// vendor pushed back was persisted with the default org: in a multi-department
// installation that put one department's paid vendor results into another
// department's quality dashboard, and it did so silently because the score count
// still went up.
//
// The rules under test, in order of authority:
//  1. the trace the score refers to (resolved against the trace store),
//  2. the org that installed the plugin (valid because dispatch is org-filtered),
//  3. neither → evals.UnattributedOrgID, never a guess.

const orgTestManifest = `apiVersion: nexus.io/v1alpha1
kind: EvalPlugin
metadata:
  name: langfuse-judge
spec:
  service:
    type: langfuse
    endpoint: https://cloud.langfuse.com
    auth:
      secretRef: langfuse-creds
      keyRef: public_key|secret_key
  send:
    trigger: on_trace
    sampling: 1.0
  collect:
    mode: webhook
    interval: 60s
    mapping:
      name: name
      score: value
      explanation: comment
      trace_id: traceId
  timeout: 30s
`

// stubOrgResolver is a fixed trace_id → org table.
type stubOrgResolver struct {
	byTrace map[string]string
	calls   int
}

func (s *stubOrgResolver) OrgForTrace(_ context.Context, traceID string) (string, bool) {
	s.calls++
	org, ok := s.byTrace[traceID]
	return org, ok
}

// collectorForOrg registers the manifest under the given org (empty = the
// cluster-wide scope a Helm-installed plugin uses) and returns a wired
// collector plus its capture sink.
func collectorForOrg(t *testing.T, orgID string) (*Collector, *fakeSink) {
	t.Helper()
	reg := evalplugin.NewRegistry()
	src := evalplugin.Source{Kind: evalplugin.SourceHelm}
	if orgID != "" {
		src = evalplugin.Source{Kind: evalplugin.SourceDatabase, Ref: "row-1"}
	}
	if discarded := reg.Merge([]evalplugin.Record{{
		Source:  src,
		Plugin:  decode(t, orgTestManifest),
		Enabled: true,
		OrgID:   orgID,
	}}); len(discarded) > 0 {
		t.Fatalf("unexpected discard: %d", len(discarded))
	}
	sink := &fakeSink{}
	c := NewCollector(reg, sink, nil)
	c.SetLogger(silentLog())
	return c, sink
}

func postScore(t *testing.T, c *Collector, traceID string) {
	t.Helper()
	body := bytes.NewBufferString(
		`{"name":"answer-relevance","value":0.9,"comment":"ok","traceId":"` + traceID + `"}`)
	if err := c.Webhook("langfuse-judge", body); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
}

// TestCollectorWebhookAttributesFromTheTrace is the primary path: a cluster-wide
// plugin has no tenant of its own, so the trace decides.
func TestCollectorWebhookAttributesFromTheTrace(t *testing.T) {
	c, sink := collectorForOrg(t, "")
	c.SetTraceOrgResolver(&stubOrgResolver{byTrace: map[string]string{"t-1": "globex"}})

	postScore(t, c, "t-1")

	if len(sink.scores) != 1 {
		t.Fatalf("want one score, got %d", len(sink.scores))
	}
	if got := sink.scores[0].OrgID; got != "globex" {
		t.Errorf("score must be attributed to the trace's org, got %q", got)
	}
}

// TestCollectorWebhookFallsBackToThePluginOrg covers the per-org install, which
// is the shape we recommend for multi-org customers precisely because it does
// not depend on the trace store being reachable.
func TestCollectorWebhookFallsBackToThePluginOrg(t *testing.T) {
	c, sink := collectorForOrg(t, "acme")
	// No resolver wired at all — the deployment has no trace store.

	postScore(t, c, "t-unknown")

	if len(sink.scores) != 1 {
		t.Fatalf("want one score, got %d", len(sink.scores))
	}
	if got := sink.scores[0].OrgID; got != "acme" {
		t.Errorf("want the installing org %q, got %q", "acme", got)
	}
}

// TestCollectorWebhookRefusesWhenTraceAndPluginDisagree: a vendor reporting on a
// trace outside the tenant whose credentials produced the result is either a
// shared vendor project pointed at two Nexus orgs or a forged trace id. Picking
// either side writes one tenant's score into another's ledger.
func TestCollectorWebhookRefusesWhenTraceAndPluginDisagree(t *testing.T) {
	c, sink := collectorForOrg(t, "acme")
	c.SetTraceOrgResolver(&stubOrgResolver{byTrace: map[string]string{"t-9": "globex"}})

	postScore(t, c, "t-9")

	if len(sink.scores) != 1 {
		t.Fatalf("want one score, got %d", len(sink.scores))
	}
	got := sink.scores[0].OrgID
	if got == "acme" || got == "globex" {
		t.Errorf("a cross-tenant mismatch must not be assigned to either side, got %q", got)
	}
	if got != evals.UnattributedOrgID {
		t.Errorf("want %q, got %q", evals.UnattributedOrgID, got)
	}
}

// TestCollectorWebhookRefusesWhenNothingIdentifiesTheTenant pins the fail-closed
// default. The row is kept (an evaluation was paid for and should be auditable)
// but lands in a scope no org's reads match.
func TestCollectorWebhookRefusesWhenNothingIdentifiesTheTenant(t *testing.T) {
	c, sink := collectorForOrg(t, "")
	c.SetTraceOrgResolver(&stubOrgResolver{byTrace: map[string]string{}})

	postScore(t, c, "t-absent")

	if len(sink.scores) != 1 {
		t.Fatalf("the score must still be persisted for audit, got %d", len(sink.scores))
	}
	if got := sink.scores[0].OrgID; got != evals.UnattributedOrgID {
		t.Errorf("want %q, got %q", evals.UnattributedOrgID, got)
	}
}

// TestCollectorWebhookTreatsLegacyDefaultSentinelAsClusterWide: rows stored
// through the console before org binding was fixed carry the "default"
// placeholder as their org. evalplugin.NormalizeOrgID maps it to cluster-wide,
// and attribution has to agree with dispatch on that or a plugin would be
// dispatched cluster-wide while its scores were filed under a literal org named
// "default".
func TestCollectorWebhookTreatsLegacyDefaultSentinelAsClusterWide(t *testing.T) {
	c, sink := collectorForOrg(t, evalplugin.LegacyDefaultOrgID)
	c.SetTraceOrgResolver(&stubOrgResolver{byTrace: map[string]string{"t-5": "globex"}})

	postScore(t, c, "t-5")

	if got := sink.scores[0].OrgID; got != "globex" {
		t.Errorf("the legacy sentinel must not outrank the trace's real org, got %q", got)
	}
}

// TestCollectorBatchWebhookAttributesEachRecordIndependently: a batched delivery
// can span traces, so attribution is per record and not per request.
func TestCollectorBatchWebhookAttributesEachRecordIndependently(t *testing.T) {
	c, sink := collectorForOrg(t, "")
	c.SetTraceOrgResolver(&stubOrgResolver{byTrace: map[string]string{
		"b-1": "acme",
		"b-2": "globex",
	}})

	body := bytes.NewBufferString(`[
		{"name":"x","value":0.4,"traceId":"b-1"},
		{"name":"x","value":0.95,"traceId":"b-2"},
		{"name":"x","value":0.5,"traceId":"b-3"}
	]`)
	if err := c.Webhook("langfuse-judge", body); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	if len(sink.scores) != 3 {
		t.Fatalf("want three scores, got %d", len(sink.scores))
	}
	want := map[string]string{
		"b-1": "acme",
		"b-2": "globex",
		"b-3": evals.UnattributedOrgID,
	}
	for _, sc := range sink.scores {
		if got := sc.OrgID; got != want[sc.TraceID] {
			t.Errorf("trace %s: want org %q, got %q", sc.TraceID, want[sc.TraceID], got)
		}
	}
}

// TestPollerKeyIsPerTenant: the running-poller set used to be keyed by plugin
// name alone. The store's uniqueness constraint is (org_id, name), so two orgs
// can legitimately install "langfuse-judge"; with a name-only key the second
// org got no poller and its vendor was never read back — a silent per-tenant
// outage that looked like "the vendor has nothing to report".
func TestPollerKeyIsPerTenant(t *testing.T) {
	a := pollerKey("acme", "langfuse-judge")
	b := pollerKey("globex", "langfuse-judge")
	if a == b {
		t.Fatal("two orgs installing the same plugin name must get distinct pollers")
	}
	if pollerKey("", "langfuse-judge") != pollerKey(evalplugin.LegacyDefaultOrgID, "langfuse-judge") {
		t.Error("the legacy default sentinel and cluster-wide must resolve to one poller, not two")
	}
}
