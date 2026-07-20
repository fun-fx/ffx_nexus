package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/balancer"
	"github.com/ffxnexus/nexus/internal/guardrails"
	"github.com/ffxnexus/nexus/internal/observability"
	"github.com/ffxnexus/nexus/internal/router"
	"github.com/ffxnexus/nexus/internal/semcache"
)

// ModelRouter ranks candidate models best-first, dropping any below minQuality.
// A nil router disables quality-aware routing. The ordered result drives both
// model selection and provider fallback.
type ModelRouter interface {
	Rank(candidates []string, minQuality float64) []string
}

// Handler serves the OpenAI-compatible gateway API and records traces.
type Handler struct {
	registry       *Registry
	recorder       observability.Recorder
	limiter        Limiter // may be nil
	router         ModelRouter
	groups         map[string][]string  // routing alias -> candidate models
	guard          *guardrails.Guard    // nil = guardrails disabled
	selfCorrectMax int                  // max structured-output self-correction retries; 0 disables
	lb             *balancer.WeightedRR // nil = no load balancing within routing tiers
	scache         *semcache.Service    // nil = semantic cache disabled
	credResolver   *CredentialResolver  // nil = no per-request BYOK resolution
	keyMode        KeyMode              // how to resolve upstream keys per request
	replicaID      string               // per-process id stamped on every Trace (multi-node grouping)
	failoverNotify router.Notifier      // optional webhook/Slack sink for router failover events (V4)
	concurrency    ConcurrencyCapIface  // V5 per-vkey in-flight cap (nil = disabled)
	log            *slog.Logger
}

// SetFailoverNotifier wires the V4 alert sinks. Nil disables — the
// handler keeps working with metrics-only failover visibility.
func (h *Handler) SetFailoverNotifier(n router.Notifier) {
	h.failoverNotify = n
}

// aliasForModel returns the routing-group name `m` resolves through
// (e.g. "fast", "auto"); empty if `m` is a concrete model id. Used only
// for failover alert enrichment so operators can pattern-match alerts
// against groups, not raw model ids.
func aliasForModel(m string, groups map[string][]string) string {
	for alias, members := range groups {
		for _, member := range members {
			if member == m {
				if alias == "auto" {
					continue // skip the synthetic default group
				}
				return alias
			}
		}
	}
	return ""
}

// notifyFailover is the V4 alert hook. It is a static-style helper so
// each failover site (handleUnary, handleStream, the legacy
// responses.go path) can call it without worrying about nil-safety or
// request-scoped context plumbing — the notifier itself never blocks.
func (h *Handler) notifyFailover(ev router.FailoverEvent) {
	if h == nil || h.failoverNotify == nil {
		return
	}
	// Use context.Background(): the request context may already be
	// cancelled by the time we get here (we're in the middle of
	// recovering from the failed primary's context deadline).
	h.failoverNotify.Notify(context.Background(), ev)
}

// SetCredentialResolution enables per-request (BYOK) upstream key resolution.
// A nil resolver or KeyModeShared keeps the legacy shared-key behavior.
func (h *Handler) SetCredentialResolution(cr *CredentialResolver, mode KeyMode) {
	h.credResolver = cr
	h.keyMode = mode
}

// ConcurrencyCapIface is the V5 per-vkey in-flight limiter contract.
// We pass it through an interface so handler.go doesn't pin against
// any single limiter implementation — handlers can be unit-tested
// with mocks. SetConcurrencyCap wires the real limiter at boot.
type ConcurrencyCapIface interface {
	Acquire(ctx context.Context, keyID string) bool
	Release(ctx context.Context, keyID string)
	Inflight(keyID string) int
}

// SetConcurrencyCap installs the V5 per-vkey in-flight cap. Pass nil
// to disable (default).
func (h *Handler) SetConcurrencyCap(c ConcurrencyCapIface) {
	h.concurrency = c
}

// NewHandler builds a gateway handler. lim may be nil.
func NewHandler(reg *Registry, rec observability.Recorder, lim Limiter, log *slog.Logger) *Handler {
	return &Handler{registry: reg, recorder: rec, limiter: lim, log: log}
}

// SetReplicaID stamps the per-process id on every Trace produced by this
// handler. In a multi-replica deployment the operator sets NEXUS_REPLICA_ID on
// each pod; the value flows through to gateway_traces.replica_id and makes
// `GROUP BY replica_id` queries meaningful for "are both replicas healthy /
// getting equal traffic?" investigations.
func (h *Handler) SetReplicaID(id string) {
	h.replicaID = id
}

// SetRouter enables quality-aware routing. groups maps an alias to candidate
// models; the built-in alias "auto" routes across all registered models.
func (h *Handler) SetRouter(r ModelRouter, groups map[string][]string) {
	h.router = r
	h.groups = groups
}

// SetGuard enables inline guardrails on the request hot path. A nil guard
// leaves guardrails disabled.
func (h *Handler) SetGuard(g *guardrails.Guard) {
	h.guard = g
}

// SetSelfCorrection enables structured-output self-correction on non-streaming
// requests: when the schema guardrail rejects a JSON response, the gateway asks
// the model to fix it (up to maxRetries times) before failing. 0 disables it.
func (h *Handler) SetSelfCorrection(maxRetries int) {
	if maxRetries < 0 {
		maxRetries = 0
	}
	h.selfCorrectMax = maxRetries
}

// SetLoadBalancing enables weighted (rank-proportional) primary selection among
// quality-qualified models in a routing alias. Failover order is preserved for
// the rest.
func (h *Handler) SetLoadBalancing(rr *balancer.WeightedRR) {
	h.lb = rr
}

// SetRouteGroups replaces routing alias -> candidate model mappings.
func (h *Handler) SetRouteGroups(groups map[string][]string) {
	if groups == nil {
		groups = map[string][]string{}
	}
	h.groups = groups
}

// RouteGroups returns a copy of the current routing alias map.
func (h *Handler) RouteGroups() map[string][]string {
	out := make(map[string][]string, len(h.groups))
	for k, v := range h.groups {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// SetSemanticCache enables embedding-based response caching on the hot path.
func (h *Handler) SetSemanticCache(s *semcache.Service) {
	h.scache = s
}

// routeCandidates returns the candidate models for a routing alias, or false if
// the requested model is a concrete model (not an alias).
func (h *Handler) routeCandidates(model string) ([]string, bool) {
	if model == "auto" {
		return h.registry.AllModels(), true
	}
	if c, ok := h.groups[model]; ok {
		return c, true
	}
	return nil, false
}

// recordSpend attributes request cost to the virtual key's monthly budget.
func (h *Handler) recordSpend(ctx context.Context, costUSD float64) {
	if h.limiter == nil || costUSD <= 0 {
		return
	}
	if vkeyID, _ := ctx.Value(ctxKeyVKeyID).(string); vkeyID != "" {
		if err := h.limiter.AddSpend(ctx, vkeyID, costUSD); err != nil {
			h.log.Warn("add spend failed", "err", err, "vkey", vkeyID)
		}
	}
}

// ChatCompletions handles POST /v1/chat/completions for both streaming and
// non-streaming requests.  It also transparently accepts Cursor Agent hybrid
// bodies that look like Responses API payloads but are posted to this path.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cannot read body: "+err.Error())
		return
	}
	// Restore body so any later middleware can re-read it.
	r.Body = io.NopCloser(bytes.NewReader(body))

	var req ChatCompletionRequest
	if IsCursorHybridRequest(body) {
		req, err = TransformCursorHybrid(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "hybrid decode: "+err.Error())
			return
		}
	} else {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
			return
		}
	}

	if req.Model == "" || len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model and messages are required")
		return
	}

	// Inline input guardrails (hot path): reject disallowed prompts before any
	// upstream call so no tokens are spent on blocked content.
	if f := h.guard.CheckInput(promptText(req.Messages)); f.Blocked {
		h.recordGuardrailBlock(r, req, f)
		writeError(w, http.StatusForbidden, "guardrail_blocked", f.Reason)
		return
	}

	// Build the ordered candidate chain. For routing aliases this is the
	// quality-ranked, min-quality-gated list; for a concrete model it is a
	// single-element chain. The gateway tries each candidate in order on
	// upstream failure (provider fallback).
	chain, ok := h.resolveChain(w, r, req)
	if !ok {
		return // resolveChain already wrote the error response
	}

	start := time.Now()
	if req.Stream {
		h.handleStream(w, r, chain, req, start)
		return
	}
	h.handleUnary(w, r, chain, req, start)
}

// resolveChain returns the ordered list of concrete model ids to attempt. It
// writes the appropriate error response and returns ok=false when the request
// cannot be served (model not allowed, no model meets quality, unknown model).
func (h *Handler) resolveChain(w http.ResponseWriter, r *http.Request, req ChatCompletionRequest) ([]string, bool) {
	if h.router != nil {
		if candidates, isAlias := h.routeCandidates(req.Model); isAlias {
			allowed := filterAllowed(r.Context(), candidates)
			if len(allowed) == 0 {
				writeError(w, http.StatusForbidden, "model_not_allowed", "this virtual key is not permitted to use any model in group "+req.Model)
				return nil, false
			}
			minQuality, _ := r.Context().Value(ctxKeyMinQuality).(float64)
			ranked := h.router.Rank(allowed, minQuality)
			if len(ranked) == 0 {
				writeError(w, http.StatusServiceUnavailable, "no_model_meets_quality",
					"no allowed model currently meets the minimum quality score for this key")
				return nil, false
			}
			if h.lb != nil {
				ranked = balancer.RotateChain(req.Model, ranked, h.lb)
			}
			h.log.Debug("routed request", "alias", req.Model, "chain", ranked, "min_quality", minQuality)
			return ranked, true
		}
	}

	if !modelAllowed(r.Context(), req.Model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "this virtual key is not permitted to use model "+req.Model)
		return nil, false
	}
	if _, _, err := h.registry.Resolve(req.Model); err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return nil, false
	}
	return []string{req.Model}, true
}

// validateOrRepairJSON validates content against the JSON/schema guardrail. If
// it fails, it attempts a free local repair (stripping markdown fences or
// surrounding prose) and re-validates. It returns the possibly-repaired content,
// the final finding, and whether a repair was applied. The repair runs only on
// the failure path, so the success path is untouched.
func (h *Handler) validateOrRepairJSON(content string, schema []byte) (string, guardrails.Finding, bool) {
	f := h.guard.CheckJSONOutput(content, schema)
	if !f.Blocked {
		return content, f, false
	}
	if fixed, ok := guardrails.RepairJSON(content); ok {
		if rf := h.guard.CheckJSONOutput(fixed, schema); !rf.Blocked {
			return fixed, rf, true
		}
	}
	return content, f, false
}

// schemaAction renders the trace guardrail_action for a structured-output
// outcome, or "" when nothing notable happened.
func schemaAction(repaired bool, corrected int) string {
	var parts []string
	if repaired {
		parts = append(parts, "json_repaired")
	}
	if corrected > 0 {
		parts = append(parts, fmt.Sprintf("self_corrected:%d", corrected))
	}
	return strings.Join(parts, ",")
}

// withSchemaCorrection appends the rejected output and a correction instruction
// to the request so the model can repair a structured-output response. It copies
// the message slice to avoid mutating the caller's request.
func withSchemaCorrection(req ChatCompletionRequest, badOutput, reason string) ChatCompletionRequest {
	msgs := make([]Message, 0, len(req.Messages)+2)
	msgs = append(msgs, req.Messages...)
	msgs = append(msgs, Message{Role: "assistant", Content: badOutput})
	msgs = append(msgs, Message{Role: "user", Content: "Your previous response was rejected: " + reason +
		" Reply with ONLY a corrected response that satisfies the requested response_format. " +
		"Do not include explanations, comments, or markdown code fences."})
	req.Messages = msgs
	return req
}

// errMissingBYOKKey signals that strict BYOK mode requires a per-user key the
// caller has not registered for this provider.
type errMissingBYOKKey struct{ provider string }

func (e errMissingBYOKKey) Error() string {
	return "no API key registered for provider " + e.provider
}

// injectCredential resolves the upstream credential for this caller+provider and
// returns a context carrying any per-request override, plus a source tag for the
// trace ("env", "org", or "user"). In KeyModeShared it is a no-op ("env"). In
// strict BYOK it returns errMissingBYOKKey when the user has no key.
func (h *Handler) injectCredential(ctx context.Context, providerName string) (context.Context, string, error) {
	if h.keyMode == KeyModeShared || h.credResolver == nil {
		return ctx, "env", nil
	}
	orgID := OrgIDFrom(ctx)
	userID := UserIDFrom(ctx)
	cred, found, err := h.credResolver.Resolve(ctx, orgID, userID, providerName)
	if err != nil {
		h.log.Warn("credential resolve failed; using shared key", "provider", providerName, "err", err)
		if h.keyMode == KeyModeStrictBYOK {
			return ctx, "", errMissingBYOKKey{provider: providerName}
		}
		return ctx, "env", nil
	}
	if !found {
		if h.keyMode == KeyModeStrictBYOK {
			return ctx, "", errMissingBYOKKey{provider: providerName}
		}
		return ctx, "env", nil // BYOK soft-fallback to shared key
	}
	ctx = WithCallerCredential(ctx, CallerCredential{
		Secret:  cred.Secret,
		BaseURL: cred.BaseURL,
		Source:  cred.Source,
	})
	return ctx, cred.Source, nil
}

func (h *Handler) handleUnary(w http.ResponseWriter, r *http.Request, chain []string, req ChatCompletionRequest, start time.Time) {
	var lastErr error
	for i, model := range chain {
		provider, fwdModel, err := h.registry.Resolve(model)
		if err != nil {
			lastErr = err
			continue
		}
		attempt := req
		attempt.Model = fwdModel

		trace := h.newTrace(r, req, provider.Name())
		trace.RequestModel = model
		attemptStart := time.Now()

		// BYOK: resolve the caller's upstream key for this provider and inject it
		// into the context the adapter reads. Shared mode is a no-op.
		callCtx, credSource, credErr := h.injectCredential(r.Context(), provider.Name())
		if credErr != nil {
			trace.LatencyMs = time.Since(attemptStart).Milliseconds()
			trace.StatusCode = http.StatusForbidden
			trace.ErrorType = "missing_byok_key"
			trace.ErrorMsg = credErr.Error()
			trace.CredentialSource = "none"
			h.recorder.Record(trace)
			lastErr = credErr
			continue
		}
		trace.CredentialSource = credSource

		// Semantic cache: only on the primary candidate, non-streaming, eligible
		// requests. Keyed by the client-requested model (req.Model), not the
		// resolved concrete model, so load-balancer rotation across the
		// quality-interchangeable members of a routing alias does not fragment the
		// cache — any qualified member's response can serve a near-duplicate prompt.
		var embedVec []float32
		if h.scache != nil && h.scache.Enabled() && i == 0 && cacheEligible(req) {
			scope := cacheScope(r.Context())
			prompt := promptText(req.Messages)
			hit, vec, err := h.scache.Lookup(r.Context(), scope, req.Model, prompt)
			switch {
			case err != nil:
				// Cache/embedding failures must never fail the request; degrade to
				// a normal upstream call but surface the error for observability.
				h.log.Warn("semantic cache lookup failed", "model", req.Model, "err", err)
			case hit != nil:
				var cached ChatCompletionResponse
				if json.Unmarshal(hit.ResponseJSON, &cached) == nil {
					trace.LatencyMs = time.Since(attemptStart).Milliseconds()
					trace.StatusCode = http.StatusOK
					trace.CacheHit = true
					trace.ResponseModel = cached.Model
					if len(cached.Choices) > 0 {
						trace.OutputMessages = cached.Choices[0].Message.Content
						trace.FinishReason = cached.Choices[0].FinishReason
					}
					h.recorder.Record(trace)
					writeJSON(w, http.StatusOK, cached)
					return
				}
			default:
				embedVec = vec // cache miss; reuse embedding for Store
			}
		}

		resp, err := provider.ChatCompletion(callCtx, attempt)
		if err != nil {
			failedAt := time.Now()
			trace.LatencyMs = time.Since(attemptStart).Milliseconds()
			trace.StatusCode = http.StatusBadGateway
			trace.ErrorType = "upstream_error"
			failover := false
			if i < len(chain)-1 {
				trace.ErrorType = "upstream_error_failover" // another candidate remains
				failover = true
			}
			trace.ErrorMsg = err.Error()
			h.recorder.Record(trace)
			// V4 alert: a primary → secondary hop happened, fan out to
			// the configured webhook / Slack sinks. We notify only when
			// we actually have a fallback (the last candidate's failure
			// is a different story — total failure, not a failover).
			if failover {
				h.notifyFailover(router.FailoverEvent{
					OrgID:        trace.OrgID,
					VirtualKeyID: trace.VirtualKeyID,
					Alias:        aliasForModel(req.Model, h.groups),
					Tried:        append([]string(nil), chain[:i+1]...),
					Primary:      chain[i],
					Fallback:     chain[i+1],
					Reason:       trace.ErrorType,
					LatencyMs:    trace.LatencyMs,
					FailedAtUnix: failedAt.UnixMilli(),
					ReplicaID:    h.replicaID,
				})
			}
			lastErr = err
			continue // fall back to the next candidate
		}

		trace.LatencyMs = time.Since(attemptStart).Milliseconds()
		trace.StatusCode = http.StatusOK
		trace.ResponseModel = resp.Model
		trace.InputTokens = resp.Usage.PromptTokens
		trace.OutputTokens = resp.Usage.CompletionTokens
		if len(resp.Choices) > 0 {
			// Output guardrails: redact PII in the response before returning it.
			if redacted, changed := h.guard.RedactOutput(resp.Choices[0].Message.Content); changed {
				resp.Choices[0].Message.Content = redacted
				trace.GuardrailAction = "output_redacted"
			}
			// Schema/JSON output guardrail (+ optional self-correction): when the
			// client requested a JSON response_format, enforce that the output is
			// valid JSON (and matches the supplied schema). When self-correction is
			// enabled, ask the model to repair a rejected response before failing.
			if req.ResponseFormat.WantsJSON() {
				schema := req.ResponseFormat.SchemaBytes()
				content, f, repaired := h.validateOrRepairJSON(resp.Choices[0].Message.Content, schema)
				resp.Choices[0].Message.Content = content
				corrected := 0
				for f.Blocked && corrected < h.selfCorrectMax {
					corrected++
					attempt = withSchemaCorrection(attempt, resp.Choices[0].Message.Content, f.Reason)
					cresp, cerr := provider.ChatCompletion(callCtx, attempt)
					if cerr != nil || len(cresp.Choices) == 0 {
						break // correction call failed; fail with the last finding
					}
					resp = cresp
					if redacted, changed := h.guard.RedactOutput(resp.Choices[0].Message.Content); changed {
						resp.Choices[0].Message.Content = redacted
					}
					trace.InputTokens += resp.Usage.PromptTokens
					trace.OutputTokens += resp.Usage.CompletionTokens
					trace.ResponseModel = resp.Model
					var rep bool
					content, f, rep = h.validateOrRepairJSON(resp.Choices[0].Message.Content, schema)
					resp.Choices[0].Message.Content = content
					repaired = repaired || rep
				}
				if action := schemaAction(repaired, corrected); action != "" {
					trace.GuardrailAction = action
				}
				if f.Blocked {
					trace.LatencyMs = time.Since(attemptStart).Milliseconds()
					trace.StatusCode = http.StatusUnprocessableEntity
					trace.ErrorType = "schema_validation_failed"
					trace.ErrorMsg = f.Reason
					trace.GuardrailAction = "output_schema_blocked:" + f.Rule
					trace.FinishReason = resp.Choices[0].FinishReason
					trace.OutputMessages = resp.Choices[0].Message.Content
					trace.CostUSD = CostUSD(trace.RequestModel, trace.InputTokens, trace.OutputTokens)
					h.recorder.Record(trace)
					h.recordSpend(r.Context(), trace.CostUSD)
					writeError(w, http.StatusUnprocessableEntity, "schema_validation_failed", f.Reason)
					return
				}
			}
			trace.FinishReason = resp.Choices[0].FinishReason
			trace.OutputMessages = resp.Choices[0].Message.Content
		}
		trace.CostUSD = CostUSD(trace.RequestModel, trace.InputTokens, trace.OutputTokens)
		if h.scache != nil && h.scache.Enabled() && i == 0 && cacheEligible(req) {
			if b, err := json.Marshal(resp); err == nil {
				if err := h.scache.Store(r.Context(), cacheScope(r.Context()), req.Model, promptText(req.Messages), embedVec, b); err != nil {
					h.log.Warn("semantic cache store failed", "model", req.Model, "err", err)
				}
			}
		}
		h.recorder.Record(trace)
		h.recordSpend(r.Context(), trace.CostUSD)

		writeJSON(w, http.StatusOK, resp)
		return
	}

	msg := "all candidate providers failed"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	writeError(w, http.StatusBadGateway, "upstream_error", msg)
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, chain []string, req ChatCompletionRequest, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	// Fallback is only possible before the first byte is written. We try to open
	// a stream for each candidate; the first that connects wins and is streamed.
	var (
		events  <-chan StreamEvent
		trace   observability.Trace
		lastErr error
	)
	for i, model := range chain {
		p, fwdModel, err := h.registry.Resolve(model)
		if err != nil {
			lastErr = err
			continue
		}
		attempt := req
		attempt.Model = fwdModel

		t := h.newTrace(r, req, p.Name())
		t.RequestModel = model
		t.Streamed = true

		callCtx, credSource, credErr := h.injectCredential(r.Context(), p.Name())
		if credErr != nil {
			t.LatencyMs = time.Since(start).Milliseconds()
			t.StatusCode = http.StatusForbidden
			t.ErrorType = "missing_byok_key"
			t.ErrorMsg = credErr.Error()
			t.CredentialSource = "none"
			h.recorder.Record(t)
			lastErr = credErr
			continue
		}
		t.CredentialSource = credSource

		ev, err := p.ChatCompletionStream(callCtx, attempt)
		if err != nil {
			t.LatencyMs = time.Since(start).Milliseconds()
			t.StatusCode = http.StatusBadGateway
			t.ErrorType = "upstream_error"
			if i < len(chain)-1 {
				t.ErrorType = "upstream_error_failover"
			}
			t.ErrorMsg = err.Error()
			h.recorder.Record(t)
			lastErr = err
			continue
		}
		events, trace = ev, t
		break
	}
	if events == nil {
		msg := "all candidate providers failed"
		if lastErr != nil {
			msg = lastErr.Error()
		}
		writeError(w, http.StatusBadGateway, "upstream_error", msg)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	trace.Streamed = true
	trace.StatusCode = http.StatusOK
	var out strings.Builder
	firstToken := true

	for evt := range events {
		switch {
		case evt.Err != nil:
			trace.ErrorType = "stream_error"
			trace.ErrorMsg = evt.Err.Error()
			// Surface the error as an SSE comment then stop.
			_, _ = w.Write([]byte(": stream error\n\n"))
			flusher.Flush()
		case evt.Done:
			// Only emit the gateway-minted [DONE] when the upstream has not
			// already sent it (passthrough providers emit Raw for the real
			// [DONE] line). This avoids a duplicate terminator.
			if evt.Raw == nil {
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
			}
		case evt.Raw != nil:
			_, _ = w.Write(evt.Raw)
			// Defensive blank separator: upstream should have ended the event
			// with "\n\n", but be tolerant with imperfect providers.
			if len(evt.Raw) > 0 && !bytes.HasSuffix(evt.Raw, []byte("\n\n")) {
				_, _ = w.Write([]byte("\n"))
			}
			flusher.Flush()

			// Side-channel metrics from the already-parsed Chunk (when the
			// provider attaches one) without ever touching a marshaler.
			if evt.Chunk != nil {
				if firstToken {
					trace.TTFTMillis = time.Since(start).Milliseconds()
					firstToken = false
				}
				if len(evt.Chunk.Choices) > 0 {
					out.WriteString(evt.Chunk.Choices[0].Delta.Content)
					if fr := evt.Chunk.Choices[0].FinishReason; fr != "" {
						trace.FinishReason = fr
					}
				}
				if evt.Chunk.Usage != nil {
					trace.InputTokens = evt.Chunk.Usage.PromptTokens
					trace.OutputTokens = evt.Chunk.Usage.CompletionTokens
				}
			}
		case evt.Chunk != nil:
			if firstToken {
				trace.TTFTMillis = time.Since(start).Milliseconds()
				firstToken = false
			}
			if len(evt.Chunk.Choices) > 0 {
				out.WriteString(evt.Chunk.Choices[0].Delta.Content)
				if fr := evt.Chunk.Choices[0].FinishReason; fr != "" {
					trace.FinishReason = fr
				}
			}
			if evt.Chunk.Usage != nil {
				trace.InputTokens = evt.Chunk.Usage.PromptTokens
				trace.OutputTokens = evt.Chunk.Usage.CompletionTokens
			}
			b, _ := json.Marshal(evt.Chunk)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}

	trace.LatencyMs = time.Since(start).Milliseconds()
	trace.OutputMessages = out.String()
	// Schema/JSON output guardrail (streaming): bytes are already sent, so we
	// cannot block. Record the violation on the trace for observability.
	if req.ResponseFormat.WantsJSON() {
		if f := h.guard.CheckJSONOutput(out.String(), req.ResponseFormat.SchemaBytes()); f.Blocked {
			trace.GuardrailAction = "output_schema_violation:" + f.Rule
		}
	}
	trace.CostUSD = CostUSD(trace.RequestModel, trace.InputTokens, trace.OutputTokens)
	h.recorder.Record(trace)
	h.recordSpend(r.Context(), trace.CostUSD)
}

// Models handles GET /v1/models.
func (h *Handler) Models(w http.ResponseWriter, r *http.Request) {
	models := h.registry.AllModels()
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(models))
	for _, m := range models {
		data = append(data, model{ID: m, Object: "model", OwnedBy: "nexus"})
	}

	// Embedding, moderation, and image models are reported under separate
	// data keys so LLM SDKs (and our console) can discover them without a
	// separate API call. Matches the convention LiteLLM and Bifrost use: a
	// single /v1/models that exposes every capability the gateway serves.
	embeddings := h.registry.AllEmbeddingModels()
	moderations := h.registry.AllModerationModels()
	images := h.registry.AllImageModels()

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
		"embeddings": map[string]any{
			"object": "list",
			"data":   toIDList(embeddings),
		},
		"moderations": map[string]any{
			"object": "list",
			"data":   toIDList(moderations),
		},
		"images": map[string]any{
			"object": "list",
			"data":   toIDList(images),
		},
	})
}

func toIDList(ids []string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "object": "model", "owned_by": "nexus"})
	}
	return out
}

func (h *Handler) newTrace(r *http.Request, req ChatCompletionRequest, providerName string) observability.Trace {
	t := observability.Trace{
		TraceID:       uuid.NewString(),
		SpanID:        uuid.NewString(),
		Timestamp:     time.Now(),
		OperationName: "chat",
		ProviderName:  providerName,
		RequestModel:  req.Model,
		ReplicaID:     h.replicaID,
	}
	if rid, ok := r.Context().Value(ctxKeyRequestID).(string); ok {
		t.ParentID = rid
	}
	if org, ok := r.Context().Value(ctxKeyOrgID).(string); ok {
		t.OrgID = org
	}
	if uid, ok := r.Context().Value(ctxKeyUserID).(string); ok {
		t.UserID = uid
	}
	if vk, ok := r.Context().Value(ctxKeyVKeyID).(string); ok {
		t.VirtualKeyID = vk
	}
	if req.Temperature != nil {
		t.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		t.TopP = *req.TopP
	}
	if req.MaxTokens != nil {
		t.MaxTokens = *req.MaxTokens
	}
	// Capture input messages (opt-in content capture; on by default in dev).
	if b, err := json.Marshal(req.Messages); err == nil {
		t.InputMessages = string(b)
	}
	if req.NexusEval != nil {
		if len(req.NexusEval.Contexts) > 0 {
			if b, err := json.Marshal(req.NexusEval.Contexts); err == nil {
				t.RetrievalContexts = string(b)
			}
		}
		t.EvalReference = req.NexusEval.Reference
	}
	return t
}

// promptText concatenates message contents into a single string for guardrail
// evaluation.
func promptText(messages []Message) string {
	var b strings.Builder
	for i, m := range messages {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Content)
	}
	return b.String()
}

// cacheEligible reports whether a request may use the semantic cache. Tool
// calls, sampling (any non-zero temperature), and RAG eval context require a
// fresh upstream call. Only deterministic requests (temperature unset or 0) are
// cacheable, so a single sampled answer is never replayed as if canonical.
func cacheEligible(req ChatCompletionRequest) bool {
	if len(req.Tools) > 0 {
		return false
	}
	if req.Temperature != nil && *req.Temperature != 0 {
		return false
	}
	if req.NexusEval != nil {
		return false
	}
	return true
}

// cacheScope returns the per-tenant namespace for semantic cache entries so one
// tenant never receives another tenant's cached response. It prefers the org id,
// falls back to the virtual key id, then to a shared "default" bucket for
// unauthenticated/zero-dependency mode.
func cacheScope(ctx context.Context) string {
	if org, _ := ctx.Value(ctxKeyOrgID).(string); org != "" {
		return org
	}
	if vk, _ := ctx.Value(ctxKeyVKeyID).(string); vk != "" {
		return vk
	}
	return "default"
}

// recordGuardrailBlock records a trace for a request rejected by an input
// guardrail (no upstream call was made).
func (h *Handler) recordGuardrailBlock(r *http.Request, req ChatCompletionRequest, f guardrails.Finding) {
	trace := h.newTrace(r, req, "")
	trace.StatusCode = http.StatusForbidden
	trace.ErrorType = "guardrail_blocked"
	trace.ErrorMsg = f.Reason
	trace.GuardrailAction = "input_blocked:" + f.Rule
	h.recorder.Record(trace)
}

// modelAllowed reports whether the authenticated key may use the model. An
// empty or absent allow-list means all models are permitted.
func modelAllowed(ctx context.Context, model string) bool {
	allowed, ok := ctx.Value(ctxKeyAllowedModels).([]string)
	if !ok || len(allowed) == 0 {
		return true
	}
	for _, m := range allowed {
		if m == model {
			return true
		}
	}
	return false
}

// filterAllowed keeps only the candidate models the virtual key may use.
func filterAllowed(ctx context.Context, candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, m := range candidates {
		if modelAllowed(ctx, m) {
			out = append(out, m)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, APIError{Error: APIErrorBody{Message: msg, Type: errType}})
}
