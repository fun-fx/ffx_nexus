package providers

import "time"

// Groq-friendly provider constructors. Mistral, Groq, The Grid, and
// other "OpenAI-compatible" providers don't need their own provider
// struct — they speak the OpenAI wire format at a different base URL.
// The defaults below mirror each vendor's published chat / embedding
// catalog as of 2026-06; admin can override by supplying an explicit
// slice to NewOpenAICompat at registration time.
//
// These constructors and constants live in the providers package so cmd/
// registration code can reference them without duplicating model lists.
// They are intentionally not wired into the gateway by default — providers
// are opt-in via env (GROQ_API_KEY, MISTRAL_API_KEY, GRID_API_KEY).

// GroqOpenAIBaseURL is the Groq OpenAI-compatible chat endpoint. Groq
// uses the same /v1/chat/completions, /v1/audio/transcriptions and
// /v1/moderations paths as OpenAI but with their own model ids.
const GroqOpenAIBaseURL = "https://api.groq.com/openai/v1"

// MistralOpenAIBaseURL is the Mistral AI OpenAI-compatible endpoint.
// Same shape as OpenAI's API; the model ids differ.
const MistralOpenAIBaseURL = "https://api.mistral.ai/v1"

// GridOpenAIBaseURL is The Grid's OpenAI-compatible consumption API.
// The Grid is an inference spot market: the model field carries an
// "instrument" string (e.g. "text-prime") instead of a model id; The
// Grid picks the supplier on each call and returns a 307 redirect to
// the actual completion endpoint. The gateway's HTTP client follows
// the redirect by default (with Authorization stripped on cross-origin
// hops — see RedirectSafeAuth in openai.go).
//
// Reference: https://thegrid.ai/docs/api-reference/consumption-api
const GridOpenAIBaseURL = "https://api.thegrid.ai/v1"

// GroqChatModels is Groq's chat-catalog as of 2026-06. Groq updates
// these frequently; admin can override by supplying an explicit slice
// to NewOpenAICompat at registration time.
var GroqChatModels = []string{
	// Llama 3.3 (preferred default)
	"llama-3.3-70b-versatile",
	"llama-3.3-70b-specdec",
	// Llama 3.1
	"llama-3.1-8b-instant",
	"llama-3.1-70b-versatile",
	// Llama 3 (legacy)
	"llama3-8b-8192",
	"llama3-70b-8192",
	// Mixtral
	"mixtral-8x7b-32768",
	// Gemma
	"gemma2-9b-it",
	// Whisper exposed via /v1/audio (separate gateway capability)
	"whisper-large-v3",
	"whisper-large-v3-turbo",
	// Guard / safety
	"llama-guard-3-8b",
}

// GroqEmbedModels is intentionally empty. Groq does not (yet) expose an
// OpenAI-compatible /v1/embeddings endpoint on the production API; they
// route embeddings through a dedicated service at a different base URL.
// We leave this nil so Groq is not advertised as embedding-capable.
var GroqEmbedModels []string

// MistralChatModels is Mistral's chat-catalog as of 2026-06.
var MistralChatModels = []string{
	"mistral-large-latest",
	"mistral-medium-latest",
	"mistral-small-latest",
	"mistral-small-2409",
	"codestral-latest",
	"codestral-2405",
	"open-mistral-7b",
	"open-mixtral-8x7b",
	"ministral-8b-latest",
	"ministral-3b-latest",
	"pixtral-12b-2409",
}

// MistralEmbedModels — Mistral exposes /v1/embeddings on the same URL.
var MistralEmbedModels = []string{
	"mistral-embed",
	"codestral-embed",
}

// GridChatModels is The Grid's instrument catalog. The Grid exposes nine
// abstract "instruments" — the consumer picks the workload shape
// (text / code / agent × standard / prime / max) and The Grid routes
// to the best-priced supplier at request time. Unlike the other
// providers here, the model id never identifies a specific LLM; it
// just names the contract tier.
//
// Reference: https://thegrid.ai/docs/instrument-specifications/current-instruments
var GridChatModels = []string{
	// Text tier
	"text-standard",
	"text-prime",
	"text-max",
	// Code tier
	"code-standard",
	"code-prime",
	"code-max",
	// Agent tier
	"agent-standard",
	"agent-prime",
	"agent-max",
}

// GridEmbedModels is intentionally empty. The Grid does not currently
// expose an OpenAI-shaped /v1/embeddings endpoint — embedding traffic
// is not part of its routing engine today.
var GridEmbedModels []string

// NewGroq registers a Groq OpenAI-compatible provider under the name "groq".
// Use the returned provider with the gateway Registry's Register call.
func NewGroq(apiKey string, timeout time.Duration) *OpenAICompat {
	return NewOpenAICompat(
		"groq", apiKey, GroqOpenAIBaseURL,
		GroqChatModels, GroqEmbedModels, nil, nil,
		timeout,
	)
}

// NewMistral registers a Mistral OpenAI-compatible provider under the
// name "mistral". Mistral exposes chat and embeddings on the same /v1
// base URL; image generation is not advertised here because Mistral's
// image service (La Plateforme) is not OpenAI-compatible.
func NewMistral(apiKey string, timeout time.Duration) *OpenAICompat {
	return NewOpenAICompat(
		"mistral", apiKey, MistralOpenAIBaseURL,
		MistralChatModels, MistralEmbedModels, nil, nil,
		timeout,
	)
}

// NewGrid registers The Grid as an OpenAI-compatible provider under the
// name "grid". The Grid's consumption API requires an "instrument" in
// the model field (no specific model ids) and replies with a 307 redirect
// to the actual supplier endpoint; the redirect policy is enforced by
// the OpenAICompat.doJSON helper, which strips the Authorization header
// on cross-origin hops so the Grid key never leaks to upstream suppliers
// in their access logs.
func NewGrid(apiKey string, timeout time.Duration) *OpenAICompat {
	return NewOpenAICompat(
		"grid", apiKey, GridOpenAIBaseURL,
		GridChatModels, GridEmbedModels, nil, nil,
		timeout,
	)
}
