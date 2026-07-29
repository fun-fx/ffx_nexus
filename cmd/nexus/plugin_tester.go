package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/console"
	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evaluators/external"
)

// PluginTester is the admin-REST "test send" backend. One Tester
// covers every installed plugin — we pick the appropriate vendor
// probe by routing on the plugin's service.type. A missing adapter
// yields a non-OK Result with a 502 SDK-unavailable message; Admins
// see the same message in the UI so they know a real vendor
// integration is the next step.
//
// We accept BOTH metadata.name and the database id (`uuid` here) so
// the UI doesn't have to know which identity the registry keys on.
// Whichever the operator typed in the URL is what they read back.
type PluginTester struct {
	reg    *evalplugin.Registry
	source *pluginSourceAdapter // for db-id lookups; nil-safe
	d      *external.Dispatcher
	c      *external.Collector
}

// Result wraps console.Result so we don't import-loop on console
// while still producing a value the route's JSON encoder emits
// unchanged. The struct fields match the JSON tags exactly.
type Result = console.Result

// Test implements console.EvalPluginTester.
//
// Sequence:
//  1. resolve {ref} (id | name) against the registry and the
//     database store; if both miss, return a friendly error.
//  2. dispatch a vendor-specific probe (LangSmith uses
//     /api/v1/info, others fall back to a HEAD against the
//     configured endpoint).
//  3. return a Result with the elapsed latency in milliseconds so
//     the operator can spot a slow egress response at a glance.
func (t *PluginTester) Test(ctx context.Context, ref string) (Result, error) {
	if t == nil || t.reg == nil {
		return Result{}, errors.New("plugin registry not initialised")
	}
	rec, ok := t.resolve(ctx, ref)
	if !ok {
		return Result{}, fmt.Errorf("plugin %q not found", ref)
	}
	start := time.Now()
	var (
		rResult Result
		rErr    error
	)
	switch rec.Plugin.Spec.Service.Type {
	case evalplugin.ServiceLangSmith:
		rErr = PingLangsmith(ctx, rec.Plugin.Spec.Service.Endpoint)
		rResult = Result{
			OK:      rErr == nil,
			Message: langSmithMessage(rErr),
		}
	default:
		rErr = genericProbe(ctx, rec.Plugin.Spec.Service.Endpoint)
		rResult = Result{
			OK:      rErr == nil,
			Message: genericMessage(string(rec.Plugin.Spec.Service.Type), rErr),
		}
	}
	rResult.LatencyMs = time.Since(start).Milliseconds()
	return rResult, nil
}

// resolve looks the plugin up by registry-map key (metadata.name),
// then by name in the database, then by database row id. It is safe
// when source is nil (e.g. unit tests), in which case only the
// registry path is exercised.
//
// The database paths matter because the registry is only populated at
// boot (LoadFromDir + LoadFromStore). A plugin an operator creates
// through the console is written straight to Postgres, so it is
// absent from the registry until the pod restarts. Resolving it from
// the store keeps "create, then press Test" working on the same page
// load instead of reporting `plugin "…" not found`.
func (t *PluginTester) resolve(ctx context.Context, ref string) (evalplugin.Record, bool) {
	if rec, ok := t.reg.Lookup(ref); ok {
		return rec, true
	}
	if t.source == nil {
		return evalplugin.Record{}, false
	}
	// Lookup is name-keyed and already falls back to the store, which
	// is how a just-created plugin resolves.
	if rec, err := t.source.Lookup(ctx, ref); err == nil {
		if out, ok := t.fromStoreRecord(rec); ok {
			return out, true
		}
	}
	// Get is id-keyed; kept so a caller holding the UUID still works.
	if rec, err := t.source.Get(ctx, ref); err == nil {
		if out, ok := t.fromStoreRecord(rec); ok {
			return out, true
		}
	}
	return evalplugin.Record{}, false
}

// fromStoreRecord turns a persisted row into a Record. It prefers the
// row's own manifest, which is what lets a plugin created since the
// last registry load resolve at all. When the row carries no manifest
// it re-keys on metadata.name in the registry, preserving the older
// behaviour for rows that only round-trip an id and a name.
func (t *PluginTester) fromStoreRecord(rec *evalplugin.PluginRecord) (evalplugin.Record, bool) {
	if rec == nil {
		return evalplugin.Record{}, false
	}
	if out, ok := recordFromStore(rec); ok {
		return out, true
	}
	if rec.Name != "" {
		if canonical, ok := t.reg.Lookup(rec.Name); ok {
			return canonical, true
		}
	}
	return evalplugin.Record{}, false
}

// recordFromStore decodes a persisted plugin row into the same
// Record shape the registry hands out, so callers downstream of
// resolve() cannot tell whether the plugin came from the in-memory
// merge or straight from Postgres.
func recordFromStore(rec *evalplugin.PluginRecord) (evalplugin.Record, bool) {
	if rec == nil || strings.TrimSpace(rec.SpecYAML) == "" {
		return evalplugin.Record{}, false
	}
	p, err := evalplugin.Decode([]byte(rec.SpecYAML))
	if err != nil {
		return evalplugin.Record{}, false
	}
	return evalplugin.Record{
		Plugin:  p,
		Source:  evalplugin.Source{Kind: evalplugin.SourceDatabase, Ref: rec.ID},
		Enabled: rec.Enabled,
	}, true
}

func newTester(reg *evalplugin.Registry,
	d *external.Dispatcher,
	c *external.Collector,
) *PluginTester {
	return &PluginTester{reg: reg, d: d, c: c}
}

// withSource attaches a DB-backed id resolver so Test() can resolve
// plugin row ids (UUID) in addition to metadata.name. Callers wire
// this from cmd/nexus/main.go after the plugin source adapter is
// constructed; nil is acceptable (e.g. in unit tests).
func (t *PluginTester) withSource(src *pluginSourceAdapter) *PluginTester {
	t.source = src
	return t
}

// genericProbe does a cheap authenticated GET against the plugin
// endpoint. We add an explicit User-Agent + Accept header so vendors
// that path-match (e.g. Datadog) don't refuse us with an unhelpful
// 400. We only learn whether the endpoint answers at all and what
// HTTP status it returned; auth headers are added by the
// vendor-specific probe path so this probe deliberately does *not*
// pretend to authenticate.
//
// Returning a typed error keeps the message round-trip lossless so
// the UI can show "endpoint <url> returned HTTP <code> (<phase>)"
// instead of a bare "HTTP 502" inherited from our own 502 response.
func genericProbe(ctx context.Context, endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("endpoint not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build probe request against %s: %w", endpoint, err)
	}
	req.Header.Set("User-Agent", "nexus-eval-plugin-tester/1.0 (+https://nexus)")
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.5")
	resp, err := httpClientForPluginsTest().Do(req)
	if err != nil {
		// Network-layer failure (DNS, TCP, TLS, timeout). Carry the
		// endpoint into the message so the operator can match it
		// against their ingress / NetworkPolicy without grepping
		// the Nexus logs. The 8-second probe timeout is intentionally
		// short so this returns well inside the Cloudflare tunnel's
		// ~60-second response deadline (Retry-After:60; cf-ray:-HKG
		// 5xx pages observed when a vendor hangs).
		return fmt.Errorf("probe %s failed at transport layer: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("endpoint %s returned HTTP %d", endpoint, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf(
			"endpoint %s reachable but rejected credentials (HTTP %d)",
			endpoint, resp.StatusCode,
		)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf(
			"endpoint %s returned HTTP %d (request malformed; check Accept header / path)",
			endpoint, resp.StatusCode,
		)
	}
	return nil
}

func langSmithMessage(err error) string {
	if err == nil {
		return "Auth accepted by LangSmith."
	}
	return fmt.Sprintf("LangSmith probe failed: %v", err)
}

func genericMessage(kind string, err error) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "endpoint"
	}
	if err == nil {
		return fmt.Sprintf("%s endpoint reachable.", kind)
	}
	return fmt.Sprintf("%s probe failed: %v", kind, err)
}
