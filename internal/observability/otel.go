package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// OTLPRecorder fans a Trace out as a JSON POST to an OTLP/HTTP endpoint
// (the receiver route that proxy-aware OpenTelemetry Collectors expose,
// e.g. `http://otel-collector:4318/v1/traces`). It is intentionally a thin
// adapter: the collector does the heavy lifting of turning the envelope
// into Prometheus remote-write, Tempo, Honeycomb, Jaeger, etc.
//
// Envelope shape: OTLP/HTTP+JSON Protobuf wire format (`ExportTraceServiceRequest`):
//
//	{
//	  "resourceSpans": [
//	    { "resource": {"attributes":[...]}, "scope_spans":[ { "scope": ..., "spans":[ ... ] } ] },
//	    ...
//	  ]
//	}
//
// Failures are loud (logged) but never block the gateway — observability
// adapters run in the hot path asynchronously, same as the CHRecorder and
// MetricsRecorder. Disable by leaving NEXUS_OTLP_ENDPOINT empty.
type OTLPRecorder struct {
	log      *slog.Logger
	endpoint string
	client   *http.Client
	ch       chan Trace
	done     chan struct{}
	wg       sync.WaitGroup
	closed   chan struct{}

	// failureHook is invoked synchronously from the flusher goroutine
	// whenever send() returns a non-nil error. Callers wire it from
	// compose.go (typically to MetricsRecorder.RecordOTLPExportFailure)
	// so the OTLP-failure rate shows up as a Prometheus series and can
	// drive an alert. Nil → no-op; we don't log every flush noise.
	failureHook func(reason string)
	// successHook fires after a successful 2xx POST. Used by the same
	// metrics recorder to track successes and byte volume; same nil
	// semantics as failureHook.
	successHook func(bytes int)
}

// OTLPOptions configures the OTLP adapter (HTTP only — gRPC is out of
// scope for V3; OTel operators typically expose proxy.otlptracegrpc on a
// different port not worth opening here).
type OTLPOptions struct {
	Endpoint    string        // full URL like http://otel-collector:4318/v1/traces
	BatchSize   int           // default 200
	FlushEvery  time.Duration // default 2s
	BufferSize  int           // default 10000
	Timeout     time.Duration // default 5s per batch send
	FailureHook func(reason string)
	SuccessHook func(bytes int)
}

// NewOTLPRecorder returns nil if Endpoint is empty (callers should treat
// nil as "disabled" and not append to MultiRecorder). Returns a running
// recorder otherwise.
func NewOTLPRecorder(opts OTLPOptions, log *slog.Logger) *OTLPRecorder {
	if opts.Endpoint == "" {
		return nil
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 200
	}
	if opts.FlushEvery == 0 {
		opts.FlushEvery = 2 * time.Second
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = 10000
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	rec := &OTLPRecorder{
		log:         log,
		endpoint:    opts.Endpoint,
		failureHook: opts.FailureHook,
		successHook: opts.SuccessHook,
		client: &http.Client{
			Timeout: opts.Timeout,
		},
		ch:     make(chan Trace, opts.BufferSize),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	rec.wg.Add(1)
	go rec.loop(opts.BatchSize, opts.FlushEvery)
	return rec
}

// Record enqueues a Trace; never blocks (a full buffer drops the trace,
// matching the ClickHouse / Prometheus semantics).
func (r *OTLPRecorder) Record(t Trace) {
	if r == nil {
		return
	}
	select {
	case r.ch <- t:
	default:
		r.log.Warn("otlp buffer full, dropping trace", "trace_id", t.TraceID)
	}
}

func (r *OTLPRecorder) loop(batchSize int, flushEvery time.Duration) {
	defer r.wg.Done()
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	buf := make([]Trace, 0, batchSize)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.send(buf); err != nil {
			r.log.Warn("otlp export failed", "err", err, "count", len(buf))
		}
		buf = buf[:0]
	}

	for {
		select {
		case <-r.done:
			for {
				select {
				case t := <-r.ch:
					buf = append(buf, t)
					if len(buf) >= batchSize {
						flush()
					}
				default:
					flush()
					close(r.closed)
					return
				}
			}
		case t := <-r.ch:
			buf = append(buf, t)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// send batches the traces into a single HTTP POST. We wrap each batch
// of Nexus traces in the OTLP/HTTP+JSON envelope (`ExportTraceServiceRequest`)
// so the collector's `otlp` receiver can ingest it directly. The bare
// Trace-array shape that we used to send (a JSON `[{"trace_id": ...}]`)
// fails with a 400 because the OTLP receiver expects a single
// ExportTraceServiceRequest object (`{"resourceSpans":[...]}`).
//
// `internal/observability/otel` only ships a JSON adapter — operators
// who need protobuf can run our collector alongside a transformation
// pipeline reusing the same envelope (deploy/observability/otel-collector-config.yml
// matches both). The JSON shape follows the OTLP spec
// (https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding).
//
// On any non-2xx response we classify the failure (`http_4xx`, `http_5xx`)
// and invoke `failureHook` so the metrics surface (`nexus_otlp_export_failures_total{reason}`)
// records it. Network errors (DNS / dial / TLS) are bucketed as `network`;
// transport-layer messaging failures (Json marshal / context cancel)
// fall under `other`. Reason-specific buckets let Grafana alerts
// distinguish "collector rejected envelope" from "collector unreachable".
func (r *OTLPRecorder) send(traces []Trace) error {
	envelope := otlpEnvelopeFromTraces(traces)
	payload, err := json.Marshal(envelope)
	if err != nil {
		r.invokeFailureHook("other", err)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		r.invokeFailureHook("other", err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		// Treat any client.Do() error (DNS, dial, TLS, conn-reset) as a
		// network-side failure; this is the bucket alerts page on when
		// the collector pod disappears.
		r.invokeFailureHook("network", err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		reason := otlpStatusReason(resp.StatusCode)
		if r.log != nil {
			r.log.Warn("otlp export failed",
				"err", errUnexpectedStatus(resp.StatusCode),
				"count", len(traces),
				"status", resp.Status,
				"body_prefix", string(body),
				"payload_bytes", len(payload),
				"payload_head", string(payload[:min(200, len(payload))]),
				"reason", reason,
			)
		}
		r.invokeFailureHook(reason, errUnexpectedStatus(resp.StatusCode))
		return errUnexpectedStatus(resp.StatusCode)
	}
	if r.successHook != nil {
		r.successHook(len(payload))
	}
	return nil
}

// invokeFailureHook fires the optional metrics hook without panicking
// if the caller wired a nil hook — keeps the send() flow non-blocking
// and consistent regardless of OTLPOptions.FailureHook being set.
func (r *OTLPRecorder) invokeFailureHook(reason string, cause error) {
	if r.failureHook == nil {
		return
	}
	defer func() {
		// Never let a hook panic kill the flusher goroutine; we'd
		// rather lose one metric increment than the whole OTLP
		// export pipeline. Note: a metrics-recorder hook should not
		// panic in practice, but defensive recovery here means an
		// operator bug never converts into a fleet-wide outage.
		_ = recover()
	}()
	r.failureHook(reason)
}

// otlpStatusReason maps an HTTP status code to the bucket name we
// surface in metrics + logs. 4xx split from 5xx so an alert can
// distinguish "envelope rejected" (likely our fix needed) from
// "collector overloaded" (likely capacity / fleet issue).
func otlpStatusReason(code int) string {
	switch {
	case code >= 400 && code < 500:
		return "http_4xx"
	case code >= 500:
		return "http_5xx"
	default:
		return "other"
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// otlpEnvelopeFromTraces returns a minimal ExportTraceServiceRequest
// carrying the given Nexus traces. Each trace maps to a single OTLP
// span under one ResourceSpans bucket, keyed by `replica_id`. Field
// names follow the OTLP/JSON spec directly (`trace_id`, `span_id`,
// `attributes`/`key`/`value`/`string_value`, etc.); the collector
// unmarshals them as Protobuf JSON and treats them as ordinary spans.
func otlpEnvelopeFromTraces(traces []Trace) map[string]any {
	resourceSpans := []map[string]any{}
	for _, t := range traces {
		attrs := filterNil([]map[string]any{
			kv("gen_ai.operation.name", stringOr(t.OperationName, "chat")),
			kv("gen_ai.provider.name", t.ProviderName),
			kv("gen_ai.request.model", t.RequestModel),
			kv("gen_ai.response.model", t.ResponseModel),
			kv("gen_ai.response.finish_reasons", t.FinishReason),
			kv("nexus.org_id", t.OrgID),
			kv("nexus.user_id", t.UserID),
			kv("nexus.virtual_key_id", t.VirtualKeyID),
			kv("nexus.credential_source", t.CredentialSource),
			kv("nexus.status_code", intToString(t.StatusCode)),
			kv("nexus.error_type", t.ErrorType),
			kv("nexus.error_message", t.ErrorMsg),
			kv("nexus.replica_id", t.ReplicaID),
			// sentinel markers so the collector can attach them as
			// numeric filterable fields; these match the Nexus-side
			// Prometheus labels used in the
			// `nexus_gateway_*` & `nexus_eval_*` dashboards.
			kv("nexus.cost_usd_micros", intToString(int(t.CostUSD*1_000_000))),
			kv("nexus.input_tokens", intToString(t.InputTokens)),
			kv("nexus.output_tokens", intToString(t.OutputTokens)),
			kv("nexus.ttft_ms", int64ToString(t.TTFTMillis)),
			kv("nexus.latency_ms", int64ToString(t.LatencyMs)),
			kv("nexus.streamed", boolToString(t.Streamed)),
			kv("nexus.cache_hit", boolToString(t.CacheHit)),
			kv("nexus.guardrail_action", t.GuardrailAction),
			kv("nexus.temperature", floatToString(t.Temperature)),
			kv("nexus.top_p", floatToString(t.TopP)),
			kv("nexus.max_tokens", intToString(t.MaxTokens)),
		})
		// OTLP requires trace_id (32 hex chars) + span_id (16 hex chars)
		// to be hex-encoded strings with no separators. Nexus upstream
		// IDs are usually UUIDs (`xxxxxxxx-xxxx-...`) when they come
		// from request_id, and 32-hex when they come from our own
		// tracing layer. We normalize both via stripDashes+hexSpanID
		// so the receiver's `parse trace_id:invalid length for ID`
		// never fires regardless of where the ID originated.
		spanID := hexSpanID(stripDashes(t.SpanID))
		if spanID == "" || spanID == "0000000000000000" {
			// SpanID was empty or got reduced to zeros; fall back to
			// the trimmed trace_id so parent/child still correlate.
			spanID = hexSpanID(stripDashes(t.TraceID))
		}
		traceID32 := stripDashes(t.TraceID)
		if len(traceID32) >= 32 {
			traceID32 = traceID32[:32]
		} else if len(traceID32) < 32 {
			const padding = "00000000000000000000000000000000"
			traceID32 = traceID32 + padding[:32-len(traceID32)]
		}
		span := map[string]any{
			"trace_id":             traceID32,
			"span_id":              spanID,
			"name":                 "gen_ai." + stringOr(t.OperationName, "chat"),
			"start_time_unix_nano": int64Or(t.Timestamp.UnixNano(), 0),
			"end_time_unix_nano":   int64Or(t.Timestamp.Add(time.Duration(t.LatencyMs)*time.Millisecond).UnixNano(), 0),
			"attributes":           attrs,
		}
		// Parent linkage: OTLP shape is parent_span_id on the child
		// span, not a SpanLink — emit one only when ParentID is present.
		// OTLP rejects `parent_span_id` that isn't exactly 16 hex
		// chars, so we run it through hexSpanID (strip dashes, pad if
		// short, take the first 16 hex digits). Without this, opening
		// a UX-side parent link to a request UUID would cause the
		// whole batch to fail with HTTP 400 from the OTLP receiver.
		if t.ParentID != "" {
			span["parent_span_id"] = hexSpanID(stripDashes(t.ParentID))
		}
		// Resource attributes: bucket-by replica; the receiver fans
		// out metrics+traces into per-replica groups downstream.
		resourceAttrs := []map[string]any{
			kv("service.name", "nexus"),
			kv("service.namespace", "gateway"),
		}
		if t.ReplicaID != "" {
			resourceAttrs = append(resourceAttrs, kv("service.instance.id", t.ReplicaID))
		}
		resourceSpans = append(resourceSpans, map[string]any{
			"resource": map[string]any{
				"attributes": resourceAttrs,
			},
			"scope_spans": []map[string]any{
				{
					"scope": map[string]any{
						"name":    "ffx_nexus",
						"version": "0.5.0",
					},
					"spans": []map[string]any{span},
				},
			},
		})
	}
	return map[string]any{"resourceSpans": resourceSpans}
}

// filterNil returns the input slice with nil entries removed; keeps
// the OTLP attribute array free of `null` placeholders that the
// receiver would reject.
func filterNil(xs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if x != nil {
			out = append(out, x)
		}
	}
	return out
}

func floatToString(f float64) string {
	if f == 0 {
		return "0"
	}
	// OTLP attribute values are strings; emit a decimal form. We
	// avoid strconv.FormatFloat because we want stable output for
	// dashboards.
	return fmtFloat(f)
}

func fmtFloat(f float64) string {
	// simple, stable decimal formatter (rounded to 4 dp)
	if f < 0 {
		return "-" + fmtFloat(-f)
	}
	intPart := int64(f)
	frac := f - float64(intPart)
	fracStr := ""
	if frac != 0 {
		fracInt := int64(frac * 10000.0)
		fracStr = "." + fmtInts(fracInt)
	}
	return fmtInts(intPart) + fracStr
}

func fmtInts(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// kv returns an OTLP attribute key/value pair as a map[string]any,
// shaped as a proto JSON {"key":"...", "value":{"string_value":"..."}}.
// Kept package-local so it composes with kafka/grpc.json round-trips.
func kv(k, v string) map[string]any {
	if v == "" {
		// Filter empty values so the rendered attribute list stays
		// compact; the collector would happily accept "" but operators
		// reading the raw json find it noisy.
		return nil
	}
	return map[string]any{
		"key":   k,
		"value": map[string]any{"string_value": v},
	}
}

func stringOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func boolToString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func int64Or(v, fallback int64) int64 {
	if v == 0 {
		return fallback
	}
	return v
}

func int64ToString(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [21]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// hexSpanID returns the first 16 hex chars of a (potentially long)
// trace id; OTLP span_id MUST be exactly 16 hex characters. We use the
// first half of the trace id so a parent/child pair stay correlated.
// Strings with non-hex characters (e.g. UUID dashes, base16 with
// stray chars) get stripped via stripDashes so the resulting id is
// guaranteed-hex where possible.
func hexSpanID(traceID string) string {
	clean := stripDashes(traceID)
	if len(clean) >= 16 {
		return clean[:16]
	}
	// pad with zeros if trace id is unexpectedly short
	const padding = "0000000000000000"
	return clean + padding[:16-len(clean)]
}

// stripDashes removes hex-disruptors from a span/trace id before we
// truncate it. OTLP's parent_span_id validator is strict: the bytes
// must be hex *and* exactly 16 chars long; passing a UUID like
// `f4b86a08-bbe4-497c-b73e-2c8e1ee86a44` (32 hex + 4 dashes = 36
// bytes) fails with `invalid length for ID`. Stripping hyphens
// shrinks it to 32 hex; hexSpanID then takes the first 16. Unknown
// non-hex characters are left in place — the receiver still rejects
// them, but in that case the operator gets the original ParseError,
// not a length error compounded.
func stripDashes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '-' && c != ' ' {
			out = append(out, c)
		}
	}
	return string(out)
}

// Close drains the buffer and stops the background flusher.
func (r *OTLPRecorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	close(r.done)
	select {
	case <-r.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unexpectedStatusError is a sentinel for non-2xx responses without
// importing errors everywhere else just for one helper.
type unexpectedStatusError struct{ code int }

func (e *unexpectedStatusError) Error() string {
	return "otlp unexpected status code " + intToString(e.code)
}

func errUnexpectedStatus(code int) error { return &unexpectedStatusError{code: code} }

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
