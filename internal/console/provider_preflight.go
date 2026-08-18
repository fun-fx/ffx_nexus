package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/core"
	"github.com/ffxnexus/nexus/internal/egress"
)

// preflightCredentialsRequest is the JSON shape the Credentials drawer
// posts to /api/me/credentials/preflight. The handler never logs or
// persists `Secret`: it is forwarded to the upstream provider inside
// a 10-second, read-only auth probe and then dropped.
type preflightCredentialsRequest struct {
	Provider string `json:"provider"`
	Secret   string `json:"secret"`
	BaseURL  string `json:"base_url,omitempty"`
}

// PreflightResult is the JSON the handler returns. `ok` is the only
// field the UI is strict about — everything else is for the operator
// to read on screen. `detected_provider` is non-empty when the
// secret shape disagrees with the dropdown choice the operator
// actually picked, so the drawer can offer a switch hint.
type PreflightResult struct {
	OK               bool   `json:"ok"`
	Provider         string `json:"provider"`
	ProviderLabel    string `json:"provider_label"`
	Status           int    `json:"status,omitempty"`
	LatencyMS        int64  `json:"latency_ms,omitempty"`
	Message          string `json:"message,omitempty"`
	DetectedProvider string `json:"detected_provider,omitempty"`
}

// preflightTimeout caps every probe at 10s. The drawer's button
// stays disabled until either ok or this deadline fires; longer
// vendor stalls are indistinguishable from outages on a UI level.
const preflightTimeout = 10 * time.Second

// providerLabels are the human-friendly names the drawer shows in
// its connection-status pill. Kept short — the pill is narrow.
var providerLabels = map[string]string{
	"openai":    "OpenAI",
	"anthropic": "Anthropic",
	"gemini":    "Google Gemini",
	"mistral":   "Mistral",
	"ollama":    "Ollama",
	"grid":      "The Grid",
}

// detectProviderFromSecret classifies a pasted secret string into a
// provider hint based on its publicly-documented prefix shape. It is
// strictly advisory: the operator's dropdown choice always wins when
// the two agree, and the drawer's "Looks like an X key" hint only
// appears when they disagree.
//
//	sk- … sk-proj- …           → openai
//	sk-ant-…                    → anthropic
//	AIza…                       → gemini
//	anything else               → "" (no detection)
func detectProviderFromSecret(secret string) string {
	s := strings.TrimSpace(secret)
	switch {
	case strings.HasPrefix(s, "sk-ant-"):
		return "anthropic"
	case strings.HasPrefix(s, "sk-proj-"), strings.HasPrefix(s, "sk-"):
		return "openai"
	case strings.HasPrefix(s, "AIza"):
		return "gemini"
	default:
		return ""
	}
}

// probeProvider is the dispatch signature every provider-specific
// function implements. The handler wraps it with the 10s timeout
// context, returns its (status, body-snippet, latency) so the result
// is uniform across the table.
type probeProvider func(ctx context.Context, secret, baseURL string) (status int, body string, err error)

// providerProbes is the closed dispatch table. Anything not in this
// map falls through to a 400 "unsupported provider".
//
// Important: each entry below is a free, read-only auth round-trip —
// no tokens are billed. The Grid is wired here for the same reason
// (its OpenAI-compatible /v1/models endpoint returns a bearer-key
// check without counting toward spend), not via a chat-completion
// probe. If you ever decide to swap that endpoint for a chat-completion
// probe you must move The Grid back out of this map.
var providerProbes = map[string]probeProvider{
	"openai":    probeOpenAI,
	"anthropic": probeAnthropic,
	"gemini":    probeGemini,
	"mistral":   probeMistral,
	"ollama":    probeOllama,
	"grid":      probeGrid,
}

// preflightCredential validates a pasted provider secret without
// ever saving it to the encrypted store. It does a single free,
// read-only auth round-trip to the upstream and reports back the
// HTTP status + the first 200 characters of the vendor's body.
//
// The handler deliberately does NOT enforce a successful probe on
// the subsequent POST /api/me/credentials call — that is a UI-level
// gating decision (see web/src/pages/Credentials.tsx). Keeping the
// two paths independent makes it trivial to disable one without the
// other during incident response and keeps server semantics simple.
func (s *Server) preflightCredential(w http.ResponseWriter, r *http.Request, u core.User) {
	var req preflightCredentialsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	secret := strings.TrimSpace(req.Secret)
	if provider == "" || secret == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "provider and secret are required",
		})
		return
	}
	probe, ok := providerProbes[provider]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "provider unsupported for pre-flight: " + provider,
		})
		return
	}
	if provider == "ollama" {
		// Ollama has no real "auth" — the secret is treated as a
		// bearer against the user's custom base URL. If the operator
		// did not supply a base URL we cannot probe anything.
		if strings.TrimSpace(req.BaseURL) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "base_url is required when provider=ollama",
			})
			return
		}
	}
	if provider == "grid" {
		// The Grid ships its own OpenAI-compatible endpoint and we
		// cannot issue a meaningful free auth round-trip against a
		// domain we don't recognise. The drawer auto-fills the
		// canonical URL on the way in, so a missing base URL here
		// almost always means the operator manually cleared it —
		// refuse rather than probe a malformed URL.
		if strings.TrimSpace(req.BaseURL) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "base_url is required when provider=grid",
			})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), preflightTimeout)
	defer cancel()
	start := time.Now()
	status, body, err := probe(ctx, secret, strings.TrimSpace(req.BaseURL))
	latencyMS := time.Since(start).Milliseconds()

	res := PreflightResult{
		Provider:      provider,
		ProviderLabel: providerLabels[provider],
		Status:        status,
		LatencyMS:     latencyMS,
	}
	switch {
	case err != nil:
		// Network error, DNS, timeout. These are not vendor rejections
		// — surface a clean message instead of a 500.
		res.OK = false
		res.Message = err.Error()
	case status >= 200 && status < 300:
		res.OK = true
		res.Message = "Connected to " + res.ProviderLabel
	default:
		res.OK = false
		res.Message = truncateErr(body)
	}
	if detected := detectProviderFromSecret(secret); detected != "" && detected != provider {
		res.DetectedProvider = detected
	}
	writeJSON(w, http.StatusOK, res)
}

// truncateErr returns at most 200 visible characters of an upstream
// error body. Vendors occasionally echo the (invalid) Authorization
// header on 401s; we don't want that in our logs or UI.
func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// --- Per-provider probes --------------------------------------------
//
// Each probe builds a single GET against a documented, free, read-only
// endpoint. They share the helper `runProbe` below so the round-trip
// code (timeout, status capture, body read with limit) lives in one
// place.

const probeBodyLimit int64 = 4096

// runProbe is the shared HTTP plumbing all provider probes use. The
// caller supplies a fully-built `*http.Request` with any provider-
// specific headers already attached, a per-request timeout via the
// passed context, and we return the upstream status + the first
// probeBodyLimit bytes of the response body. The body is read via
// io.LimitReader so a vendor returning a 4MB error page cannot slow
// the handler down.
func runProbe(ctx context.Context, req *http.Request) (int, string, error) {
	// Tenant class: for the ollama and grid providers the probe target is the
	// base_url in the request body, so any authenticated user can name it. The
	// status code and the first probeBodyLimit bytes of the response are returned
	// to that user, which makes this a general-purpose read of anything the pod
	// can reach unless the destination is checked.
	client := egress.Client(egress.Tenant, preflightTimeout)
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, probeBodyLimit)
	body, _ := io.ReadAll(lr)
	return resp.StatusCode, string(body), nil
}

func probeOpenAI(ctx context.Context, secret, _ string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	return runProbe(ctx, req)
}

func probeAnthropic(ctx context.Context, secret, _ string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet,
		"https://api.anthropic.com/v1/models?limit=1", nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("x-api-key", secret)
	req.Header.Set("anthropic-version", "2023-06-01")
	return runProbe(ctx, req)
}

func probeGemini(ctx context.Context, secret, _ string) (int, string, error) {
	// Gemini accepts the API key as a `key` query parameter rather
	// than an Authorization header. Encoding it through net/url avoids
	// having to worry about characters in the secret that would break
	// the URL — Google documents the API key as URL-safe ASCII today
	// but a future key shape could include characters that need
	// percent-encoding.
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models?pageSize=1&key=" + url.QueryEscape(secret)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, "", err
	}
	return runProbe(ctx, req)
}

func probeMistral(ctx context.Context, secret, _ string) (int, string, error) {
	// Mistral's docs allow either raw or "Bearer …" for the secret;
	// the drawer does not enforce either, so we normalize.
	cleaned := strings.TrimPrefix(secret, "Bearer ")
	cleaned = strings.TrimSpace(cleaned)
	req, err := http.NewRequest(http.MethodGet, "https://api.mistral.ai/v1/models", nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+cleaned)
	return runProbe(ctx, req)
}

func probeOllama(ctx context.Context, _, baseURL string) (int, string, error) {
	// Ollama has no auth — the endpoint test is connectivity-only.
	// We hit /api/tags because it is the cheapest documented probe
	// (zero inference cost) and exists on every Ollama release that
	// supports tool calls.
	req, err := http.NewRequest(http.MethodGet,
		strings.TrimRight(baseURL, "/")+"/api/tags", nil)
	if err != nil {
		return 0, "", err
	}
	return runProbe(ctx, req)
}

// probeGrid authenticates against The Grid's OpenAI-compatible
// /v1/models endpoint. The grid bills no tokens for that call —
// its model-list endpoint is a free read-only check, identical in
// shape to OpenAI's, which is why this entry is allowed inside the
// providerProbes map. The drawer auto-fills baseURL with the canonical
// Grid consumption URL (always /v1-suffixed) so trimming the trailing
// slash and appending /models yields the correct path whether the
// operator supplied the API root with or without a trailing slash.
func probeGrid(ctx context.Context, secret, baseURL string) (int, string, error) {
	cleaned := strings.TrimSpace(secret)
	cleaned = strings.TrimPrefix(cleaned, "Bearer ")
	cleaned = strings.TrimSpace(cleaned)
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+cleaned)
	return runProbe(ctx, req)
}
