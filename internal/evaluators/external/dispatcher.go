// Package external implements the eval plugin dispatcher. The
// dispatcher turns each loaded EvalPlugin record into one
// evals.Evaluator instance the worker can fan out scores from.
//
// Plugins are config-only: the dispatcher never inlines the vendor's
// SDK. Each adapter is a small encoding-only layer (HTTP send +
// optional flat-key decode for sync results). Polling and webhook
// collection are handled by Collector, which feeds results back into
// the registry through Sink writers.
package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ffxnexus/nexus/internal/egress"
	"github.com/ffxnexus/nexus/internal/evalplugin"
	"github.com/ffxnexus/nexus/internal/evals"
	"github.com/ffxnexus/nexus/internal/observability"
)

// TransmitFunc is the per-vendor send hook implemented by adapters.
// It receives the fully-rendered payload (after redaction) plus a
// Target carrying the endpoint, the resolved credentials, the manifest
// and the trace. Adapters translate that into the vendor's wire shape
// and POST it.
type TransmitFunc func(ctx context.Context, tgt Target, payload map[string]any) error

// Dispatcher fans out traces to plugin transmittters. Async results
// are routed through a Collector instance — the dispatcher itself
// doesn't poll or accept webhooks. The historical "sync" inline
// result path was retired (see validate.go's validCollectMode).
type Dispatcher struct {
	mu       sync.Mutex
	reg      *evalplugin.Registry
	client   *http.Client
	transmit map[evalplugin.ServiceType]TransmitFunc
	secrets  SecretResolver
}

// NewDispatcher builds a Dispatcher. callers must Register all
// adapters they'll use before calling Dispatch for the first time.
func NewDispatcher(reg *evalplugin.Registry, httpClient *http.Client) *Dispatcher {
	if httpClient == nil {
		// Tenant class: the destination is the plugin manifest's endpoint and the
		// payload carries rendered trace content.
		httpClient = egress.Client(egress.Tenant, 30*time.Second)
	}
	return &Dispatcher{
		reg:      reg,
		client:   httpClient,
		transmit: make(map[evalplugin.ServiceType]TransmitFunc),
	}
}

// SetSecretResolver wires the resolver used to turn a manifest's
// spec.service.auth into concrete credentials. Without it, plugins
// that declare auth fail dispatch with ErrNoSecretResolver rather than
// sending an unauthenticated request the vendor will reject.
func (d *Dispatcher) SetSecretResolver(r SecretResolver) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.secrets = r
}

// Register attaches an outbound transmit function for a service type.
// Adapter packages call this on init()/Register() so the Dispatcher
// can be constructed without an explicit adapter list.
func (d *Dispatcher) Register(t evalplugin.ServiceType, fn TransmitFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.transmit[t] = fn
}

// Transmit exposes a read-only snapshot of registered transmit hooks.
// Tests use this to assert adapter registration without racing the
// Dispatcher's internal mutex.
func (d *Dispatcher) Transmit() map[evalplugin.ServiceType]TransmitFunc {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[evalplugin.ServiceType]TransmitFunc, len(d.transmit))
	for k, v := range d.transmit {
		out[k] = v
	}
	return out
}

// ForPlugin constructs a one-shot Evaluator for a single plugin. The
// returned Evaluator's Name() is `plugin:<metadata.name>` which is
// what the score's Evaluator field will hold.
func ForPlugin(p *evalplugin.Plugin, dispatcher *Dispatcher) evals.Evaluator {
	return &pluginEvaluator{plugin: p, dispatcher: dispatcher}
}

type pluginEvaluator struct {
	plugin     *evalplugin.Plugin
	dispatcher *Dispatcher
}

// Name implements evals.Evaluator.
func (e *pluginEvaluator) Name() string { return "plugin:" + e.plugin.Metadata.Name }

// Evaluate implements evals.Evaluator. Sampling, redaction and HTTP
// dispatch are handled in Dispatch; we return nil scores here and
// let the collector decode results when they come back. The hot
// path never touches a network round-trip — adapters do, in their
// own Transmit hook.
func (e *pluginEvaluator) Evaluate(ctx context.Context, t observability.Trace) ([]evals.Score, error) {
	if err := e.dispatcher.Dispatch(ctx, t, e.plugin); err != nil {
		return nil, err
	}
	return nil, nil
}

// Dispatch renders the payload and hands it to the registered transmit
// function. Errors propagate so worker logs make them visible.
func (d *Dispatcher) Dispatch(ctx context.Context, t observability.Trace, p *evalplugin.Plugin) error {
	if p == nil {
		return errors.New("nil plugin")
	}
	d.mu.Lock()
	fn := d.transmit[p.Spec.Service.Type]
	resolver := d.secrets
	d.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("no adapter registered for service.type=%s", p.Spec.Service.Type)
	}
	rendered, err := renderPayload(p.Spec.Send.Payload, t)
	if err != nil {
		return fmt.Errorf("render payload: %w", err)
	}
	if err := redactPayload(rendered, t, p.Spec.Send.Redact); err != nil {
		return fmt.Errorf("redact payload: %w", err)
	}
	creds, err := resolveAuth(ctx, resolver, p)
	if err != nil {
		return fmt.Errorf("resolve auth for plugin %s: %w", p.Metadata.Name, err)
	}
	return fn(ctx, Target{
		Endpoint: p.Spec.Service.Endpoint,
		Auth:     creds,
		Plugin:   p,
		Trace:    t,
	}, rendered)
}

// renderPayload applies the user's Go text/template to each value in
// the payload map. Failure to render any single field surfaces as
// an error so the caller can drop the trace rather than forward a
// half-baked payload.
func renderPayload(tmpls map[string]string, t observability.Trace) (map[string]any, error) {
	ctx := traceContext(t)
	if len(tmpls) == 0 {
		return map[string]any{"trace": ctx}, nil
	}
	out := make(map[string]any, len(tmpls))
	for k, v := range tmpls {
		if !strings.Contains(v, "{{") {
			out[k] = v
			continue
		}
		tmpl, err := template.New(k).Option("missingkey=zero").Parse(v)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", k, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("execute template %s: %w", k, err)
		}
		out[k] = buf.String()
	}
	return out, nil
}

// traceContext turns a Trace into a map[string]any so callers can
// use either struct-field syntax (`{{ .trace.InputMessages }}`) or
// map-key syntax (`{{ index .trace "input_messages" }}`) in their
// templates. JSON tags on observability.Trace already map the
// lowercase form, so YAML authors can pick either style.
func traceContext(t observability.Trace) map[string]any {
	b, _ := json.Marshal(t)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return map[string]any{"trace": out}
}

// redactPayload scans rendered payload string fields for PII
// patterns and replaces hits with `[REDACTED:<kind>]`. Only "pii" is
// supported today; unknown kinds are rejected at Validate time so
// the kind slice here is closed.
func redactPayload(payload map[string]any, _ observability.Trace, kinds []string) error {
	shouldRedact := false
	for _, k := range kinds {
		if k == "pii" {
			shouldRedact = true
			break
		}
	}
	if !shouldRedact {
		return nil
	}
	// The heuristic PII detector ships in internal/evals. We don't
	// import it (would create an evals → evaluators/external cycle);
	// re-implement the conservative patterns here so the dispatcher
	// is self-contained.
	for k, v := range payload {
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = redactPII(s)
		payload[k] = s
	}
	return nil
}

// reRedactEmail mirrors internal/evals/heuristics.go's reEmail. The
// pattern is duplicated rather than imported because internal/evals
// depends on this package, and RE2 gives us a single linear pass with
// no backtracking — which matters because this runs on whole prompts.
var reRedactEmail = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// redactPII masks the cheap-to-detect patterns. The full regex-set
// lives in internal/evals/heuristics.go; duplicating here keeps the
// dispatcher independent of the worker import graph so it can be
// loaded in unit tests without spinning the gateway.
//
// This replaced a hand-rolled scanner that walked '@' positions in the
// input while searching for the next one in the partially-masked
// output. Once those two indices diverged the replacement became a
// no-op and the position stopped advancing, so any prompt that merely
// mentioned '@' as prose — "reference files using the @ symbol" — span
// forever at full CPU inside the eval worker, holding the goroutine
// with no error to report. Anchoring on a regex keeps the pass bounded
// by construction, and only masks spans that actually look like an
// address instead of mangling a bare '@'.
func redactPII(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	return reRedactEmail.ReplaceAllString(s, "[REDACTED:email]")
}

// Apply writes an externally-produced score (incoming webhook /
// polled fetch) into the standard sink. Adapters call this once
// they have decoded the flat-key mapping back into the canonical
// OTel shape.
//
// After writing to the sink, Apply also emits the score as a
// `gen_ai.evaluation.result` OTLP event when an EvaluationLogSink
// has been configured via SetEvaluationLogSink. The default is a
// no-op sink so the dispatcher's evaluated-trace hot path is
// unchanged in deployments that haven't configured
// NEXUS_OTLP_LOGS_ENDPOINT.
func Apply(raw json.RawMessage, mapping evalplugin.ResultMapping, pluginName string) (evals.Score, error) {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return evals.Score{}, fmt.Errorf("decode vendor json: %w", err)
	}
	name := pickString(parsed, mapping.Name, mapping.Metric, "name")
	score, _ := pickFloat(parsed, mapping.Score, 0)
	label := pickString(parsed, mapping.Label)
	explanation := pickString(parsed, mapping.Explanation, "comment")
	traceID := pickString(parsed, mapping.TraceID, "trace_id")
	passed := score >= 0.5
	if label != "" {
		lower := strings.ToLower(label)
		if lower == "pass" || lower == "true" || lower == "1" {
			passed = true
		} else if lower == "fail" || lower == "false" || lower == "0" {
			passed = false
		}
	}
	return evals.Score{
		TraceID:   traceID,
		Evaluator: "plugin:" + pluginName,
		Metric:    name,
		Score:     score,
		Passed:    passed,
		Rationale: explanation,
		Timestamp: time.Now().UTC(),
	}, nil
}

func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func pickFloat(m map[string]any, key string, fallback float64) (float64, bool) {
	if key == "" {
		return fallback, false
	}
	v, ok := m[key]
	if !ok {
		return fallback, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f, true
		}
	}
	return fallback, false
}

// ReadBody is shared with adapters that need to limit response body
// size to avoid memory blowups in debugging scenarios.
func ReadBody(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 1 << 20
	}
	limited := io.LimitReader(r, max)
	buf := new(bytes.Buffer)
	n, err := io.Copy(buf, limited)
	if err != nil {
		return nil, err
	}
	if n >= max {
		return buf.Bytes(), errors.New("body too large")
	}
	return buf.Bytes(), nil
}
