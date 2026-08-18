package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
)

// CollectFunc is the per-vendor result-fetcher implemented by
// adapters. It is called once per polling tick; the returned
// JSON.RawMessage is fed through Apply to produce an evals.Score.
//
// Adapters receive the same Target as TransmitFunc, minus a trace
// (polling is not tied to one), because reading results back needs the
// vendor credentials just as much as writing does.
type CollectFunc func(ctx context.Context, tgt Target) ([]json.RawMessage, error)

// Collector drives the "collect" side of each plugin. It runs a
// background goroutine per polling-mode plugin and exposes the
// Webhook receiver endpoint for webhook-mode plugins.
//
// Webhook mode is intentionally bare: the receiver is a single
// function that decrypts the signed payload, parses it via the
// plugin's ResultMapping, and drops the resulting Score into the
// supplied Sink. There is no per-plugin endpoint granularity —
// admins configure one shared signing key per installation.
type Collector struct {
	mu       sync.Mutex
	reg      *evalplugin.Registry
	collects map[evalplugin.ServiceType]CollectFunc
	sink     evals.Sink
	client   *http.Client
	secrets  SecretResolver
	log      *slog.Logger
	evSink   evals.OTLPEvaluationLogSink
	// orgs resolves a vendor-supplied trace_id to the organisation that
	// owns the trace. Nil is tolerated (see attributeOrg) but leaves
	// cluster-wide plugins unable to attribute their scores.
	orgs TraceOrgResolver
	// running tracks the poll goroutine per plugin name so the
	// supervisor can start pollers for plugins created after boot and
	// stop them once the plugin is deleted or disabled.
	running map[string]context.CancelFunc
}

// TraceOrgResolver answers "which organisation owns this trace".
//
// The collect side of a plugin has no request context to inherit a tenant
// from: a webhook is an unauthenticated inbound POST from LangSmith or
// Langfuse, and a poll is a background tick. The only tenant signal in the
// payload is the trace_id the vendor echoes back, so resolving it against the
// trace store is what keeps a vendor's scores inside the tenant whose traffic
// produced them.
type TraceOrgResolver interface {
	// OrgForTrace returns the owning org and true, or false when the trace
	// is unknown. An error is reported as (—, false) by implementations;
	// the collector treats "unknown" and "lookup failed" identically
	// because both mean the same thing here: do not guess.
	OrgForTrace(ctx context.Context, traceID string) (string, bool)
}

// NewCollector builds a Collector. The sink is the same instance
// the worker uses (ClickHouse or Postgres) so plugin-generated
// scores share storage with heuristic- and judge-generated scores.
func NewCollector(reg *evalplugin.Registry, sink evals.Sink, httpClient *http.Client) *Collector {
	if httpClient == nil {
		// Tenant class: the poll destination is the plugin's endpoint, and the
		// response is written into eval_scores.
		httpClient = egress.Client(egress.Tenant, 30*time.Second)
	}
	return &Collector{
		reg:      reg,
		sink:     sink,
		client:   httpClient,
		collects: make(map[evalplugin.ServiceType]CollectFunc),
		running:  make(map[string]context.CancelFunc),
		evSink:   evals.NoopLogSink{},
	}
}

// SetSecretResolver wires credential resolution for the polling path.
func (c *Collector) SetSecretResolver(r SecretResolver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.secrets = r
}

// SetLogger attaches a logger so poll and sink failures are visible.
func (c *Collector) SetLogger(l *slog.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = l
}

// SetTraceOrgResolver wires trace→org resolution for inbound vendor scores.
// Leave unset only in deployments with no trace store, where there are no
// traces to attribute against; cluster-wide plugins then write their scores as
// evals.UnattributedOrgID.
func (c *Collector) SetTraceOrgResolver(r TraceOrgResolver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgs = r
}

// SetEvaluationLogSink installs the OTLP sink that every score
// Apply() produces will be fanned out to. Leave nil to install
// the noop sink (default). The fan-out happens after WriteScores,
// so a sink failure cannot lose a score that's already persisted
// to ClickHouse/Postgres; it just won't be mirrored to OTLP-aware
// collectors.
func (c *Collector) SetEvaluationLogSink(s evals.OTLPEvaluationLogSink) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s == nil {
		s = evals.NoopLogSink{}
	}
	c.evSink = s
}

// Register attaches a CollectFunc for a vendor. Mirrors
// Dispatcher.Register's ergonomics.
func (c *Collector) Register(t evalplugin.ServiceType, fn CollectFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.collects[t] = fn
}

// Run starts the per-plugin pollers and blocks until ctx is done.
// Each polling-mode plugin spawns its own goroutine. Webhook
// receivers don't need a goroutine — they listen on the configured
// HTTP routes from the console mux.
func (c *Collector) Run(ctx context.Context) error {
	// The registry is filled at boot but also mutated at runtime when an
	// operator creates or deletes a plugin in the console. Re-scanning
	// keeps polling in step with that: a one-shot scan meant a plugin
	// created after startup never had a poller until the pod restarted.
	c.syncPollers(ctx)
	ticker := time.NewTicker(pollSupervisorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.syncPollers(ctx)
		}
	}
}

// pollSupervisorInterval is how often Run reconciles running pollers
// against the registry. Short enough that a newly installed plugin
// starts collecting promptly, long enough to be free of consequence.
const pollSupervisorInterval = 30 * time.Second

// syncPollers starts a poll goroutine for every enabled poll-mode
// plugin that lacks one, and cancels goroutines whose plugin is gone.
//
// The running set is keyed by (org, name), not name alone. Two organisations
// may install a poll-mode plugin under the same name — the store's uniqueness
// constraint is on the (org_id, name) pair — and a name-only key gave the
// second one no poller at all, so that tenant's vendor was never read back.
func (c *Collector) syncPollers(ctx context.Context) {
	wanted := make(map[string]evalplugin.Record)
	for _, rec := range c.reg.Enabled() {
		if rec.Plugin == nil || rec.Plugin.Spec.Collect.Mode != "poll" {
			continue
		}
		wanted[pollerKey(rec.OrgID, rec.Plugin.Metadata.Name)] = rec
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for key, cancel := range c.running {
		if _, keep := wanted[key]; !keep {
			cancel()
			delete(c.running, key)
		}
	}
	for key, rec := range wanted {
		if _, active := c.running[key]; active {
			continue
		}
		interval := rec.Plugin.Spec.Collect.Interval.Std()
		if interval <= 0 {
			interval = 60 * time.Second
		}
		pollCtx, cancel := context.WithCancel(ctx)
		c.running[key] = cancel
		go c.runPoll(pollCtx, rec.Plugin, rec.OrgID, interval)
	}
}

// pollerKey namespaces a running poller by tenant. The separator cannot appear
// in an org id or a plugin metadata.name (both are validated identifiers), so
// the composite is unambiguous.
func pollerKey(orgID, name string) string {
	return evalplugin.NormalizeOrgID(orgID) + "\x00" + name
}

func (c *Collector) runPoll(ctx context.Context, p *evalplugin.Plugin, pluginOrg string, interval time.Duration) {
	c.mu.Lock()
	fn := c.collects[p.Spec.Service.Type]
	resolver := c.secrets
	c.mu.Unlock()
	if fn == nil {
		c.logf("plugin poll has no adapter", "plugin", p.Metadata.Name,
			"service_type", string(p.Spec.Service.Type))
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			creds, err := resolveAuth(ctx, resolver, p)
			if err != nil {
				c.logf("plugin poll auth failed", "plugin", p.Metadata.Name, "err", err)
				continue
			}
			payloads, err := fn(ctx, Target{
				Endpoint: p.Spec.Service.Endpoint,
				Auth:     creds,
				Plugin:   p,
			})
			if err != nil {
				// Swallowing this is what made a misconfigured plugin
				// look like a plugin with nothing to report.
				c.logf("plugin poll failed", "plugin", p.Metadata.Name, "err", err)
				continue
			}
			c.applyAllForOrg(ctx, p, pluginOrg, payloads)
		}
	}
}

// logf emits a warning when a logger is wired, and is a no-op
// otherwise so tests can construct a bare Collector.
func (c *Collector) logf(msg string, args ...any) {
	if c.log == nil {
		return
	}
	c.log.Warn(msg, args...)
}

// applyAll converts each fetched JSON record into a Score and
// writes them via the supplied Sink. Adapter implementations
// should keep payloads small; we cap at 1000 per tick as a
// safety net against pathological backfills.
func (c *Collector) applyAll(ctx context.Context, p *evalplugin.Plugin, payloads []json.RawMessage) {
	c.applyAllForOrg(ctx, p, "", payloads)
}

// applyAllForOrg is applyAll with the plugin's own tenant in hand.
//
// pluginOrg is the org the plugin record is installed under; the empty string
// means cluster-wide. It is passed in rather than read off the Plugin because
// tenancy lives on the registry Record, not on the decoded manifest.
func (c *Collector) applyAllForOrg(ctx context.Context, p *evalplugin.Plugin, pluginOrg string, payloads []json.RawMessage) {
	if c.sink == nil || len(payloads) == 0 {
		return
	}
	if len(payloads) > 1000 {
		payloads = payloads[:1000]
	}
	scores := make([]evals.Score, 0, len(payloads))
	for _, raw := range payloads {
		sc, err := Apply(raw, p.Spec.Collect.Mapping, p.Metadata.Name)
		if err != nil {
			continue
		}
		sc.OrgID = c.attributeOrg(ctx, p.Metadata.Name, pluginOrg, sc.TraceID)
		scores = append(scores, sc)
	}
	if len(scores) == 0 {
		return
	}
	if err := c.sink.WriteScores(ctx, scores); err != nil {
		c.logf("plugin score write failed", "plugin", p.Metadata.Name,
			"count", len(scores), "err", err)
		return
	}
	c.fanOutEvalEvents(ctx, scores)
}

// attributeOrg decides which organisation an inbound vendor score belongs to.
//
// The trace is the authority: a score describes one call, and that call's org
// was fixed when the gateway served it. The plugin's own org is the fallback,
// valid because dispatch is org-filtered (Registry.EnabledForOrg) so a per-org
// plugin only ever receives that org's traces and can only be reporting on
// them.
//
// When the two disagree the score is refused rather than assigned to either.
// That combination means a vendor sent a result for a trace outside the tenant
// whose plugin and credentials produced it — either a misconfigured shared
// vendor project or a forged trace_id — and picking a side would either hide
// the anomaly or write one tenant's score into another's ledger.
func (c *Collector) attributeOrg(ctx context.Context, pluginName, pluginOrg, traceID string) string {
	pluginOrg = evalplugin.NormalizeOrgID(pluginOrg)

	traceOrg := ""
	c.mu.Lock()
	resolver := c.orgs
	c.mu.Unlock()
	if resolver != nil && strings.TrimSpace(traceID) != "" {
		if org, ok := resolver.OrgForTrace(ctx, traceID); ok {
			traceOrg = evalplugin.NormalizeOrgID(org)
		}
	}

	switch {
	case traceOrg != "" && pluginOrg != "" && traceOrg != pluginOrg:
		c.logf("plugin score crosses a tenant boundary; refusing attribution",
			"plugin", pluginName, "plugin_org", pluginOrg, "trace_org", traceOrg,
			"org_written", evals.UnattributedOrgID)
		return evals.UnattributedOrgID
	case traceOrg != "":
		return traceOrg
	case pluginOrg != "":
		return pluginOrg
	default:
		// Cluster-wide plugin, trace not resolvable. Do not guess.
		c.logf("plugin score could not be attributed to an organisation",
			"plugin", pluginName, "trace_id_present", strings.TrimSpace(traceID) != "",
			"resolver_wired", resolver != nil, "org_written", evals.UnattributedOrgID)
		return evals.UnattributedOrgID
	}
}

// fanOutEvalEvents sends each successfully-persisted score into
// the OTLP sink SetEvaluationLogSink wires. Failures here cannot
// lose a score that's already in the durable sink; they're logged
// at warn level because the only way to reach this path is for
// the OTLP endpoint to be down — an actionable condition.
func (c *Collector) fanOutEvalEvents(ctx context.Context, scores []evals.Score) {
	sink := c.evalLogSink()
	if sink == nil {
		return
	}
	for i := range scores {
		err := sink.EmitShip(ctx, scores[i].TraceID, scores[i])
		if err != nil {
			c.logf("otlp evaluation event emit failed",
				"plugin", "see_call_site", "err", err)
		}
	}
}

func (c *Collector) evalLogSink() evals.OTLPEvaluationLogSink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evSink
}

// Webhook processes a single inbound HTTP request from an external
// service. Adapters don't take part in this path — the unmarshal +
// ResultMapping plumbing is identical regardless of vendor. The
// caller (console mux) is responsible for verifying the request
// signature before invoking.
func (c *Collector) Webhook(pluginName string, body io.Reader) error {
	if c.sink == nil {
		return errors.New("sink is not configured")
	}
	rec, ok := c.reg.Lookup(pluginName)
	if !ok {
		return fmt.Errorf("plugin %q not loaded", pluginName)
	}
	if rec.Plugin.Spec.Collect.Mode != "webhook" {
		return fmt.Errorf("plugin %q is not in webhook collect mode", pluginName)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	// Accept either a single object or an array; multi-record
	// deliveries (batched webhooks) split before applying.
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		arr = []json.RawMessage{raw}
	}
	// rec.OrgID is the tenant that installed this plugin, or "" when it came
	// from Helm and is shared. Threading it through is what lets a per-org
	// plugin attribute its scores without a trace lookup.
	c.applyAllForOrg(context.Background(), rec.Plugin, rec.OrgID, arr)
	return nil
}

// helperString is a defensive wrapper around strings.HasPrefix used
// by adapters when checking vendor-specific response content-types.
func helperString(s, prefix string) bool { return strings.HasPrefix(s, prefix) }

// httpStatusError wraps a non-2xx HTTP response so the caller can
// inspect status without re-querying resp.
type httpStatusError struct{ Status int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("unexpected HTTP status %d", e.Status) }

// ensureResponse is the shared "did we succeed?" check used by
// adapters to keep status-class handling consistent.
func ensureResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &httpStatusError{Status: resp.StatusCode}
}
