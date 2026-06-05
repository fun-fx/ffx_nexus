// Package config loads runtime configuration from environment variables.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the Nexus gateway.
type Config struct {
	// HTTP
	GatewayAddr string // gateway proxy listen address
	ConsoleAddr string // console API / dashboard listen address

	// Datastores. Empty values disable the corresponding integration so the
	// core gateway can boot with zero dependencies (Bifrost-style).
	PostgresURL   string
	ClickHouseURL string // native protocol DSN, e.g. clickhouse://user:pass@host:9000/db
	RedisURL      string

	// Provider credentials.
	OpenAIAPIKey    string
	OpenAIBaseURL   string
	AnthropicAPIKey string
	GeminiAPIKey    string

	// MasterKey is the KEK used to encrypt provider credentials at rest. Inject
	// from a secret manager/KMS in production. Empty disables the credential
	// store (gateway then uses provider keys from env only).
	MasterKey string

	// Eval judge (local SLM via OpenAI-compatible inference server).
	JudgeBaseURL   string // e.g. http://localhost:11434/v1 (Ollama) or vLLM
	JudgeModel     string
	JudgeAPIKey    string  // optional bearer token for the inference server
	EvalSampleRate float64 // 0..1, fraction of traces sent to the SLM judge
	EvalWorkers    int     // concurrent eval goroutines

	// External Python eval service (DeepEval/RAGAS sidecar). Empty URL disables.
	// Runs out-of-band like the SLM judge; failures degrade to Go heuristics.
	EvalServiceURL     string
	EvalServiceMetrics string // comma-separated metric ids
	EvalServiceTimeout time.Duration

	// Quality-aware routing (Phase 4). RouteGroups maps an alias to candidate
	// models, e.g. "fast=gpt-4o-mini,gemini-2.5-flash;smart=gpt-4o,claude-...".
	// The built-in alias "auto" always routes across all registered models.
	RouteGroups   string
	RouteWQuality float64
	RouteWCost    float64
	RouteWLatency float64
	RouteWindow   time.Duration
	RouteRefresh  time.Duration

	// Inline guardrails (hot path). Synchronous policy checks that can block a
	// request before the upstream call or redact the response after it.
	GuardrailsEnabled     bool
	GuardrailBlockPIIIn   bool
	GuardrailRedactPIIOut bool
	GuardrailMaxInputChrs int
	GuardrailDenyPatterns string // semicolon-separated regular expressions
	GuardrailValidateJSON bool   // enforce JSON/schema on responses with a JSON response_format

	// Structured-output self-correction (hot path, non-streaming). When the
	// schema guardrail rejects a JSON response, the gateway asks the model to
	// repair it up to MaxRetries times before failing.
	SelfCorrectionEnabled    bool
	SelfCorrectionMaxRetries int

	// Observability
	OTLPEnabled bool

	// Behavior
	UpstreamTimeout time.Duration
}

// Load reads configuration from the environment, applying sane defaults. It
// first loads a local .env file (if present) for developer convenience; real
// environment variables always take precedence over .env entries.
func Load() Config {
	loadDotEnv(".env")
	return Config{
		GatewayAddr:     env("NEXUS_GATEWAY_ADDR", ":8080"),
		ConsoleAddr:     env("NEXUS_CONSOLE_ADDR", ":8081"),
		PostgresURL:     env("NEXUS_POSTGRES_URL", ""),
		ClickHouseURL:   env("NEXUS_CLICKHOUSE_URL", ""),
		RedisURL:        env("NEXUS_REDIS_URL", ""),
		OpenAIAPIKey:    env("OPENAI_API_KEY", ""),
		OpenAIBaseURL:   env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		AnthropicAPIKey: env("ANTHROPIC_API_KEY", ""),
		GeminiAPIKey:    env("GEMINI_API_KEY", ""),
		MasterKey:       env("NEXUS_MASTER_KEY", ""),
		JudgeBaseURL:    env("NEXUS_JUDGE_BASE_URL", ""),
		JudgeModel:      env("NEXUS_JUDGE_MODEL", "qwen2.5:7b"),
		JudgeAPIKey:     env("NEXUS_JUDGE_API_KEY", ""),
		EvalSampleRate:  envFloat("NEXUS_EVAL_SAMPLE_RATE", 1.0),
		EvalWorkers:     envInt("NEXUS_EVAL_WORKERS", 4),

		EvalServiceURL:     env("NEXUS_EVAL_SERVICE_URL", ""),
		EvalServiceMetrics: env("NEXUS_EVAL_SERVICE_METRICS", "answer_relevancy,toxicity,bias"),
		EvalServiceTimeout: envDuration("NEXUS_EVAL_SERVICE_TIMEOUT", 30*time.Second),
		RouteGroups:        env("NEXUS_ROUTE_GROUPS", ""),
		RouteWQuality:      envFloat("NEXUS_ROUTE_W_QUALITY", 0.6),
		RouteWCost:         envFloat("NEXUS_ROUTE_W_COST", 0.2),
		RouteWLatency:      envFloat("NEXUS_ROUTE_W_LATENCY", 0.2),
		RouteWindow:        envDuration("NEXUS_ROUTE_WINDOW", time.Hour),
		RouteRefresh:       envDuration("NEXUS_ROUTE_REFRESH", 30*time.Second),
		OTLPEnabled:        envBool("NEXUS_OTLP_ENABLED", false),
		UpstreamTimeout:    envDuration("NEXUS_UPSTREAM_TIMEOUT", 120*time.Second),

		GuardrailsEnabled:     envBool("NEXUS_GUARDRAILS_ENABLED", false),
		GuardrailBlockPIIIn:   envBool("NEXUS_GUARDRAILS_BLOCK_PII_INPUT", false),
		GuardrailRedactPIIOut: envBool("NEXUS_GUARDRAILS_REDACT_PII_OUTPUT", false),
		GuardrailMaxInputChrs: envInt("NEXUS_GUARDRAILS_MAX_INPUT_CHARS", 0),
		GuardrailDenyPatterns: env("NEXUS_GUARDRAILS_DENY_PATTERNS", ""),
		GuardrailValidateJSON: envBool("NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT", false),

		SelfCorrectionEnabled:    envBool("NEXUS_SELF_CORRECTION_ENABLED", false),
		SelfCorrectionMaxRetries: envInt("NEXUS_SELF_CORRECTION_MAX_RETRIES", 1),
	}
}

// loadDotEnv reads KEY=VALUE lines from path and sets them in the process
// environment only if the variable is not already set. Lines starting with '#'
// and blank lines are ignored. Surrounding quotes on values are stripped.
// A missing file is not an error; this is a developer convenience for local
// E2E testing and is never the mechanism used in production.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
