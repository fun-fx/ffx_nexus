package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/observability"
)

// ModelRouter selects the best concrete model from a set of candidates. A nil
// router disables quality-aware routing.
type ModelRouter interface {
	Select(candidates []string) (string, bool)
}

// Handler serves the OpenAI-compatible gateway API and records traces.
type Handler struct {
	registry *Registry
	recorder observability.Recorder
	limiter  Limiter // may be nil
	router   ModelRouter
	groups   map[string][]string // routing alias -> candidate models
	log      *slog.Logger
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

	// Quality-aware routing: if the requested model is a routing alias (e.g.
	// "auto" or a configured group), pick the best concrete model among the
	// candidates the key is allowed to use.
	if h.router != nil {
		if candidates, isAlias := h.routeCandidates(req.Model); isAlias {
			allowed := filterAllowed(r.Context(), candidates)
			if len(allowed) == 0 {
				writeError(w, http.StatusForbidden, "model_not_allowed", "this virtual key is not permitted to use any model in group "+req.Model)
				return
			}
			if selected, ok := h.router.Select(allowed); ok {
				h.log.Debug("routed request", "alias", req.Model, "selected", selected)
				req.Model = selected
			}
		}
	}

	if !modelAllowed(r.Context(), req.Model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "this virtual key is not permitted to use model "+req.Model)
		return
	}

	provider, fwdModel, err := h.registry.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	trace := h.newTrace(r, req, provider.Name())
	req.Model = fwdModel // forward de-prefixed model id to the provider
	start := time.Now()

	if req.Stream {
		h.handleStream(w, r, provider, req, trace, start)
		return
	}
	h.handleUnary(w, r, provider, req, trace, start)
}

func (h *Handler) handleUnary(w http.ResponseWriter, r *http.Request, p Provider, req ChatCompletionRequest, trace observability.Trace, start time.Time) {
	resp, err := p.ChatCompletion(r.Context(), req)
	if err != nil {
		trace.LatencyMs = time.Since(start).Milliseconds()
		trace.StatusCode = http.StatusBadGateway
		trace.ErrorType = "upstream_error"
		trace.ErrorMsg = err.Error()
		h.recorder.Record(trace)
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	trace.LatencyMs = time.Since(start).Milliseconds()
	trace.StatusCode = http.StatusOK
	trace.ResponseModel = resp.Model
	trace.InputTokens = resp.Usage.PromptTokens
	trace.OutputTokens = resp.Usage.CompletionTokens
	if len(resp.Choices) > 0 {
		trace.FinishReason = resp.Choices[0].FinishReason
		trace.OutputMessages = resp.Choices[0].Message.Content
	}
	trace.CostUSD = CostUSD(trace.RequestModel, trace.InputTokens, trace.OutputTokens)
	h.recorder.Record(trace)
	h.recordSpend(r.Context(), trace.CostUSD)

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, p Provider, req ChatCompletionRequest, trace observability.Trace, start time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "streaming unsupported")
		return
	}

	events, err := p.ChatCompletionStream(r.Context(), req)
	if err != nil {
		trace.LatencyMs = time.Since(start).Milliseconds()
		trace.StatusCode = http.StatusBadGateway
		trace.ErrorType = "upstream_error"
		trace.ErrorMsg = err.Error()
		h.recorder.Record(trace)
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
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
	return t
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
