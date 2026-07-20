// Package gateway: model-list fetchers used by the dynamic model sync worker.
// Each fetcher calls the provider's upstream /v1/models endpoint (or the
// provider-specific equivalent) and returns the discovered model id set.
//
// Design notes:
//
//   - Fetchers are stdlib-only (net/http + encoding/json). Pulling in an
//     SDK just for "which models exist" would balloon the dependency graph
//     for a payload the gateway already has to handle for the actual chat
//     / embedding calls.
//
//   - Fetchers take a context.Context; the worker cancels it on shutdown
//     and the http.Client respects it via NewRequestWithContext. The
//     Client.Timeout is set per-call as a safety net (the upstream could
//     hang even after ctx fires because of a stale socket).
//
//   - Fetchers NEVER mutate registry state. They return a plain slice and
//     let the worker decide how to apply it (single producer, single
//     consumer pattern).
//
//   - Errors are returned for the worker to log/retry; we deliberately do
//     NOT panic, log, or sleep inside the fetcher so it stays trivially
//     testable with httptest.NewServer.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ModelFetcher pulls the live model id list from an upstream provider. The
// returned slice owns the slice it returns (caller may store or mutate).
type ModelFetcher func(ctx context.Context) ([]string, error)

// NewOpenAIModelFetcher hits <baseURL>/v1/models with the Bearer key and
// returns the providers' advertised model ids. Empty apiKey returns an
// error so the worker logs a config issue exactly once instead of making
// unauthenticated calls that would always 401.
func NewOpenAIModelFetcher(apiKey, baseURL string, timeout time.Duration) ModelFetcher {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return func(ctx context.Context) ([]string, error) {
		if apiKey == "" {
			return nil, fmt.Errorf("openai: api key not configured")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
		if err != nil {
			return nil, fmt.Errorf("openai: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return fetchModelList(newHTTPClient(timeout), req, "openai")
	}
}

// NewAnthropicModelFetcher hits <baseURL>/v1/models with x-api-key. Anthropic
// does not currently expose /v1/models on api.anthropic.com, so the fetcher
// returns an explicit "not supported" error and the worker logs it as a
// configuration point — this is intentional drift-tolerance so adding a new
// provider does not silently wedge the registry.
func NewAnthropicModelFetcher(apiKey, baseURL string, timeout time.Duration) ModelFetcher {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return func(ctx context.Context) ([]string, error) {
		if apiKey == "" {
			return nil, fmt.Errorf("anthropic: api key not configured")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
		if err != nil {
			return nil, fmt.Errorf("anthropic: build request: %w", err)
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		// Anthropic has historically only published a static model card,
		// not /v1/models. Treating the 404 / 405 as a hard-cap on the
		// catalog (static list returned by the builtin adapter) avoids
		// pretending the live list is dynamic when it is not.
		models, err := fetchModelList(newHTTPClient(timeout), req, "anthropic")
		if err != nil {
			return nil, fmt.Errorf("anthropic: %w (provider has no /v1/models endpoint; falling back to builtin static list)", err)
		}
		return models, nil
	}
}

// NewGeminiModelFetcher hits <baseURL>/models?key=<apiKey>. Gemini's
// model list endpoint accepts the key as a query parameter; we mirror
// the documented format so a misconfigured base URL fails fast with a
// 404 instead of getting stuck behind a generic gateway error.
func NewGeminiModelFetcher(apiKey, baseURL string, timeout time.Duration) ModelFetcher {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return func(ctx context.Context) ([]string, error) {
		if apiKey == "" {
			return nil, fmt.Errorf("gemini: api key not configured")
		}
		u := baseURL + "/models?key=" + apiKey + "&pageSize=1000"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("gemini: build request: %w", err)
		}
		req.Header.Set("x-goog-api-key", apiKey)
		// Gemini's response shape is {"models":[{...,"name":"models/<id>"}]}; we
		// strip the "models/" prefix so the catalog id matches what the rest of
		// the gateway advertises (e.g. "gemini-2.5-flash" not
		// "models/gemini-2.5-flash").
		models, err := fetchGeminiModelList(newHTTPClient(timeout), req)
		if err != nil {
			return nil, fmt.Errorf("gemini: %w", err)
		}
		return models, nil
	}
}

// newHTTPClient returns a Client with a per-call timeout. We deliberately do
// NOT share a Transport across fetchers because each provider can have very
// different latency profiles (Gemini is fast; OpenAI can be slow under load)
// and a shared pool would cap concurrent refreshes to its idle size.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// modelsResponse matches the OpenAI/Anthropic shape ({"data":[{"id":"..."},...]}).
// Other compatible providers (Together, OpenRouter, ...) use the same field
// names so this struct works for them too. If a provider disagrees on the
// shape we add a dedicated decoder below.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// fetchModelList runs req and decodes the standard OpenAI-compatible
// response. The provider name is only used for error wrapping.
func fetchModelList(client *http.Client, req *http.Request, provider string) ([]string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Limit the body read so we don't store a 10MB HTML error page in a log
		// line. The first 256 bytes are usually enough to recognise "401 invalid
		// key" vs "404 not found" vs "503 overloaded".
		cap := io.LimitReader(resp.Body, 256)
		body, _ := io.ReadAll(cap)
		return nil, fmt.Errorf("upstream %s: %s: %s", provider, resp.Status, strings.TrimSpace(string(body)))
	}
	var out modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("upstream %s returned zero models", provider)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID == "" {
			continue
		}
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// geminiModelsResponse matches Gemini's list-models payload. "name" carries
// the fully-qualified model path "models/<id>"; we strip the prefix so the
// advertised ids match what chat/embed callers send in `model`.
type geminiModelsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

func fetchGeminiModelList(client *http.Client, req *http.Request) ([]string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		cap := io.LimitReader(resp.Body, 256)
		body, _ := io.ReadAll(cap)
		return nil, fmt.Errorf("upstream gemini: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out geminiModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(out.Models) == 0 {
		return nil, fmt.Errorf("upstream gemini returned zero models")
	}
	ids := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" || id == m.Name {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}
