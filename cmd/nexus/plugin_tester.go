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
	// secrets resolves the plugin's auth block so a probe can exercise
	// the vendor's authenticated surface. Without it a probe only
	// proves the host answers HTTP, which is how a plugin with wrong
	// credentials reported "endpoint reachable" while every real
	// dispatch was being rejected.
	secrets external.SecretResolver

	// langSmithLastKey is the credential the most recent LangSmith
	// probe carried, if any. The test-result message reflects it so
	// the operator can tell "connectivity + valid key" apart from
	// "connectivity only".
	langSmithLastKey string
	// braintrustLastKey mirrors langSmithLastKey for the Braintrust
	// probe. A single Bearer value is the only shape Braintrust
	// accepts on /v1/projects; recording it lets the result message
	// distinguish "Auth accepted" from "endpoint reachable".
	braintrustLastKey string
	// datadogLastKey mirrors the same pattern for Datadog's
	// /api/v1/validate probe (DD-API-KEY header).
	datadogLastKey string
	// confidentLastPair records the (primary, secondary) we sent for
	// Confident AI; both hold the Bearer path when only one token is
	// present, both hold Basic when a public|secret pair is present.
	confidentLastPair confidentPair
	// arizePhoenixLastPair records what we carried for the Arize
	// Phoenix probe — same shape as Confident AI but Phoenix also
	// allows the unauthenticated self-host case (both fields empty).
	arizePhoenixLastPair confidentPair
}

// confidentPair records what credential a vendor probe attached so
// the result message can distinguish "Auth accepted" from
// "endpoint reachable (no key attached)". For vendors that accept
// either a single key (Bearer) or a pair (Basic) — Confident AI and
// Arize Phoenix — exactly one of the two paths is filled in per
// probe. For self-hosted Phoenix with no auth, both fields are
// empty (that is also a valid configuration).
type confidentPair struct {
	primary   string
	secondary string
	hasAny    bool
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
// Verified thresholds (last live check on 2026-08-03) for the four
// remaining live-vendor probes added by the same fix as PR #195:
//
//   langSmithCarriedKey() -> "Auth accepted by LangSmith." on 2xx
//   braintrustKey held in t.braintrustLastKey, attaches Bearer on /v1/projects
//   datadogKey held in t.datadogLastKey, attaches DD-API-KEY on /api/v1/validate
//   confidentAIPrimary/Secondary held in t.confidentLastPair, ping attaches Basic/Bearer
//   arizePhoenixPrimary/Secondary held in t.arizePhoenixLastPair, ping attaches Basic/Bearer on /v1/traces
//
// Earlier Releases routed these four vendors to genericProbe,
// returning a bare connectivity result; the failures of that
// shortcut are documented in the individual Ping* funcs.
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
		rErr = t.probeLangsmith(ctx, rec.Plugin)
		rResult = Result{
			OK:      rErr == nil,
			Message: langSmithMessage(rErr, t.probeLangsmithCarriedKey()),
		}
	case evalplugin.ServiceLangfuse:
		rErr = t.probeLangfuse(ctx, rec.Plugin)
		rResult = Result{
			OK:      rErr == nil,
			Message: langfuseMessage(rErr),
		}
	case evalplugin.ServiceConfidentAI:
		rErr = t.probeConfidentAI(ctx, rec.Plugin)
		rResult = Result{
			OK:      rErr == nil,
			Message: confidentAIMessage(rErr, t.probeConfidentAICarried()),
		}
	case evalplugin.ServiceBraintrust:
		rErr = t.probeBraintrust(ctx, rec.Plugin)
		rResult = Result{
			OK:      rErr == nil,
			Message: braintrustMessage(rErr, t.probeBraintrustCarriedKey()),
		}
	case evalplugin.ServiceArizePhoenix:
		rErr = t.probeArizePhoenix(ctx, rec.Plugin)
		rResult = Result{
			OK:      rErr == nil,
			Message: arizePhoenixMessage(rErr, t.probeArizePhoenixCarried()),
		}
	case evalplugin.ServiceDatadog:
		rErr = t.probeDatadog(ctx, rec.Plugin)
		rResult = Result{
			OK:      rErr == nil,
			Message: datadogMessage(rErr, t.probeDatadogCarriedKey()),
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

// probeLangsmith verifies both connectivity *and* the resolved
// credential. It mirrors probeLangfuse's shape: pull the secret out
// of the vault, attach it in the header the vendor's REST docs ask
// for (`x-api-key` for LangSmith), and treat a 2xx as the only
// acceptable answer.
//
// The earlier probe ignored the HTTP status and ignored the side
// effect of pasting no key — both of which let "Test" pass against
// a 401-only host. That is the failure mode this fix replaces.
func (t *PluginTester) probeLangsmith(ctx context.Context, p *evalplugin.Plugin) error {
	auth := p.Spec.Service.Auth
	if auth.SecretRef == "" && auth.KeyRef == "" {
		return errors.New("no auth configured: LangSmith requires a LangChain API key")
	}
	if t.secrets == nil {
		return external.ErrNoSecretResolver
	}
	creds, err := t.secrets.Resolve(ctx, auth)
	if err != nil {
		return err
	}
	t.langSmithLastKey = creds.Primary()
	return PingLangsmith(ctx, p.Spec.Service.Endpoint, t.langSmithLastKey)
}

// probeLangsmithCarriedKey returns whatever key the most recent probe
// attached. The Test-result message uses it to tell the operator
// whether the request actually carried a credential rather than a
// bare connectivity check.
func (t *PluginTester) probeLangsmithCarriedKey() string {
	if t == nil {
		return ""
	}
	return t.langSmithLastKey
}

func (t *PluginTester) probeLangfuse(ctx context.Context, p *evalplugin.Plugin) error {
	auth := p.Spec.Service.Auth
	if auth.SecretRef == "" && auth.KeyRef == "" {
		return errors.New("no auth configured: Langfuse requires a public key and a secret key")
	}
	if t.secrets == nil {
		return external.ErrNoSecretResolver
	}
	creds, err := t.secrets.Resolve(ctx, auth)
	if err != nil {
		return err
	}
	pub, secret, ok := creds.Pair()
	if !ok {
		return errors.New("resolved only one credential: set auth.keyRef to both keys, " +
			"e.g. keyRef: public_key|secret_key")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	url := joinEndpoint(p.Spec.Service.Endpoint, langfuseScoresPath) + "?limit=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(pub, secret)
	resp, err := httpClientForPluginsTest().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("credentials rejected (%s): check the public/secret key pair "+
			"and that they belong to this Langfuse project", resp.Status)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("scores API not found (%s): this Langfuse version predates "+
			"the v3 scores API", resp.Status)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

func langfuseMessage(err error) string {
	if err == nil {
		return "Langfuse authenticated: endpoint and API keys verified."
	}
	return "Langfuse probe failed: " + err.Error()
}

// probeConfidentAI pulls the resolved credential and hands it to
// PingConfidentAI. Either Bearer (single key) or Basic (pair) is
// recorded in t.confidentLastPair so the result message can confirm
// what was actually carried. We refuse nothing — if the operator
// configured auth but the vault failed to resolve it, PingConfidentAI
// itself returns the descriptive error.
func (t *PluginTester) probeConfidentAI(ctx context.Context, p *evalplugin.Plugin) error {
	auth := p.Spec.Service.Auth
	if auth.SecretRef == "" && auth.KeyRef == "" {
		return errors.New("no auth configured: Confident AI requires an " +
			"API key or a public|secret key pair")
	}
	if t.secrets == nil {
		return external.ErrNoSecretResolver
	}
	creds, err := t.secrets.Resolve(ctx, auth)
	if err != nil {
		return err
	}
	primary, secondary, _ := creds.Pair()
	if primary == "" {
		primary = creds.Primary()
	}
	t.confidentLastPair = confidentPair{primary: primary, secondary: secondary}
	if primary != "" || secondary != "" {
		t.confidentLastPair.hasAny = true
	}
	return PingConfidentAI(ctx, p.Spec.Service.Endpoint, primary, secondary)
}

func (t *PluginTester) probeConfidentAICarried() confidentPair {
	if t == nil {
		return confidentPair{}
	}
	return t.confidentLastPair
}

// probeBraintrust pulls the resolved bearer and hands it to
// PingBraintrust. The carried-key is recorded so the result message
// tells the operator whether we actually presented credentials.
func (t *PluginTester) probeBraintrust(ctx context.Context, p *evalplugin.Plugin) error {
	auth := p.Spec.Service.Auth
	if auth.SecretRef == "" && auth.KeyRef == "" {
		return errors.New("no auth configured: Braintrust requires an API key")
	}
	if t.secrets == nil {
		return external.ErrNoSecretResolver
	}
	creds, err := t.secrets.Resolve(ctx, auth)
	if err != nil {
		return err
	}
	key := creds.Primary()
	if key == "" {
		return errors.New("resolved only an empty credential for Braintrust")
	}
	t.braintrustLastKey = key
	return PingBraintrust(ctx, p.Spec.Service.Endpoint, key)
}

func (t *PluginTester) probeBraintrustCarriedKey() string {
	if t == nil {
		return ""
	}
	return t.braintrustLastKey
}

// probeArizePhoenix handles three configs: unauthenticated self-host
// (both empty), Basic (space_id|api_key pair), Bearer (single key).
// Whatever bearer we attach is recorded for the result message.
func (t *PluginTester) probeArizePhoenix(ctx context.Context, p *evalplugin.Plugin) error {
	auth := p.Spec.Service.Auth
	if auth.SecretRef == "" && auth.KeyRef == "" {
		// Self-host without auth is allowed.
		t.arizePhoenixLastPair = confidentPair{}
		return PingArizePhoenix(ctx, p.Spec.Service.Endpoint, "", "")
	}
	if t.secrets == nil {
		return external.ErrNoSecretResolver
	}
	creds, err := t.secrets.Resolve(ctx, auth)
	if err != nil {
		return err
	}
	primary, secondary, _ := creds.Pair()
	if primary == "" {
		primary = creds.Primary()
	}
	t.arizePhoenixLastPair = confidentPair{primary: primary, secondary: secondary}
	if primary != "" || secondary != "" {
		t.arizePhoenixLastPair.hasAny = true
	}
	return PingArizePhoenix(ctx, p.Spec.Service.Endpoint, primary, secondary)
}

func (t *PluginTester) probeArizePhoenixCarried() confidentPair {
	if t == nil {
		return confidentPair{}
	}
	return t.arizePhoenixLastPair
}

// probeDatadog pulls a single DD-API-KEY and hands it to PingDatadog.
func (t *PluginTester) probeDatadog(ctx context.Context, p *evalplugin.Plugin) error {
	auth := p.Spec.Service.Auth
	if auth.SecretRef == "" && auth.KeyRef == "" {
		return errors.New("no auth configured: Datadog requires a DD-API-KEY")
	}
	if t.secrets == nil {
		return external.ErrNoSecretResolver
	}
	creds, err := t.secrets.Resolve(ctx, auth)
	if err != nil {
		return err
	}
	key := creds.Primary()
	if key == "" {
		return errors.New("resolved only an empty credential for Datadog")
	}
	t.datadogLastKey = key
	return PingDatadog(ctx, p.Spec.Service.Endpoint, key)
}

func (t *PluginTester) probeDatadogCarriedKey() string {
	if t == nil {
		return ""
	}
	return t.datadogLastKey
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

// withSecrets attaches the credential resolver so vendor probes can
// exercise an authenticated endpoint. nil is acceptable, in which case
// those probes report ErrNoSecretResolver rather than passing on a
// weaker unauthenticated check.
func (t *PluginTester) withSecrets(r external.SecretResolver) *PluginTester {
	t.secrets = r
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

func langSmithMessage(err error, carriedKey string) string {
	if err == nil {
		// The vendor returned 2xx while presenting the resolved key —
		// distinguish that from a bare connectivity pass since
		// operators have asked us, repeatedly, which one happened.
		if carriedKey != "" {
			return "Auth accepted by LangSmith."
		}
		return "LangSmith endpoint reachable (no key attached — Test cannot confirm credentials)."
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

// confidentAIMessage mirrors langSmithMessage — the operator needs
// to know whether the "Test" pass came from a real credential check
// or whether the resolver yielded nothing. carriedAny == false means
// no credential was carried (host answered in some unspecified
// shape); true means we attached Basic or Bearer.
func confidentAIMessage(err error, carried confidentPair) string {
	if err == nil {
		if carried.hasAny {
			return "Confident AI authenticated: endpoint and API keys verified."
		}
		return "Confident AI endpoint reachable (no key attached — Test cannot confirm credentials)."
	}
	return fmt.Sprintf("Confident AI probe failed: %v", err)
}

func braintrustMessage(err error, carriedKey string) string {
	if err == nil {
		if carriedKey != "" {
			return "Braintrust authenticated: endpoint and API key verified."
		}
		return "Braintrust endpoint reachable (no key attached — Test cannot confirm credentials)."
	}
	return fmt.Sprintf("Braintrust probe failed: %v", err)
}

func arizePhoenixMessage(err error, carried confidentPair) string {
	if err == nil {
		switch {
		case carried.hasAny:
			return "Arize Phoenix authenticated: endpoint and credentials verified."
		default:
			return "Arize Phoenix endpoint reachable (no auth configured — fine for self-host; hosted Phoenix requires a credential)."
		}
	}
	return fmt.Sprintf("Arize Phoenix probe failed: %v", err)
}

func datadogMessage(err error, carriedKey string) string {
	if err == nil {
		if carriedKey != "" {
			return "Datadog authenticated: endpoint and DD-API-KEY verified."
		}
		return "Datadog endpoint reachable (no key attached — Test cannot confirm credentials)."
	}
	return fmt.Sprintf("Datadog probe failed: %v", err)
}
