package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/balancer"
	"github.com/ffxnexus/nexus/internal/guardrails"
	"github.com/ffxnexus/nexus/internal/observability"
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
	log            *slog.Logger
}

// NewHandler builds a gateway handler. lim may be nil.
func NewHandler(reg *Registry, rec observability.Recorder, lim Limiter, log *slog.Logger) *Handler {
	return &Handler{registry: reg, recorder: rec, limiter: lim, log: log}
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
// non-streaming requests.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
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

		resp, err := provider.ChatCompletion(r.Context(), attempt)
		if err != nil {
			trace.LatencyMs = time.Since(attemptStart).Milliseconds()
			trace.StatusCode = http.StatusBadGateway
			trace.ErrorType = "upstream_error"
			if i < len(chain)-1 {
				trace.ErrorType = "upstream_error_failover" // another candidate remains
			}
			trace.ErrorMsg = err.Error()
			h.recorder.Record(trace)
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
					cresp, cerr := provider.ChatCompletion(r.Context(), attempt)
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

		ev, err := p.ChatCompletionStream(r.Context(), attempt)
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
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
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
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) newTrace(r *http.Request, req ChatCompletionRequest, providerName string) observability.Trace {
	t := observability.Trace{
		TraceID:       uuid.NewString(),
		SpanID:        uuid.NewString(),
		Timestamp:     time.Now(),
		OperationName: "chat",
		ProviderName:  providerName,
		RequestModel:  req.Model,
	}
	if rid, ok := r.Context().Value(ctxKeyRequestID).(string); ok {
		t.ParentID = rid
	}
	if org, ok := r.Context().Value(ctxKeyOrgID).(string); ok {
		t.OrgID = org
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
