// Package config loads runtime configuration from environment variables.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the Nexus gateway.
type Config struct {
	// HTTP
	GatewayAddr      string // gateway proxy listen address
	ConsoleAddr      string // console API / dashboard listen address
	PublicGatewayURL string // user-facing base URL shown in the console (optional)

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
	GroqAPIKey      string
	MistralAPIKey   string
	GridAPIKey      string

	// MasterKey is the KEK used to encrypt provider credentials at rest. Inject
	// from a secret manager/KMS in production. Empty disables the credential
	// store (gateway then uses provider keys from env only).
	MasterKey string

	// EvalEnabled toggles the async eval worker (heuristics + optional judges).
	EvalEnabled bool

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

	// --- V5 high-concurrency tuning -------------------------------------
	// MaxConcurrentPerKey caps *concurrent* in-flight requests per virtual
	// key, on a single replica. Use it to keep one noisy virtual key
	// from starving others at the upstream provider queue. 0 = disable.
	MaxConcurrentPerKey int
	RouteWCost          float64
	RouteWLatency       float64
	RouteWindow         time.Duration
	RouteRefresh        time.Duration

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

	// Load balancing within routing tiers (round-robin primary among qualified models).
	RouteLoadBalance bool

	// Semantic cache (Redis + embeddings). Requires Redis and an embeddings endpoint.
	SemanticCacheEnabled    bool
	SemanticCacheTTL        time.Duration
	SemanticCacheThreshold  float64
	SemanticCacheMaxEntries int
	EmbeddingsURL           string
	EmbeddingsModel         string
	EmbeddingsAPIKey        string
	EmbeddingsTimeout       time.Duration

	// Observability: every Trace flows out through one or more
	// adapters. Each adapter is independent, so a sink failure can
	// never block the others. All three toggles are *opt-in* so the
	// zero-dep fast path (no ClickHouse, no Prometheus, no OTLP) stays
	// unchanged for plain `nexus` boots.
	OTLPEnabled bool
	// OTLPEndpoint is the full OTLP/HTTP URL the exporter POSTs each
	// batch to (e.g. http://otel-collector:4318/v1/traces or
	// https://api.honeycomb.io/v1/traces). An empty endpoint is a NO-OP
	// even when OTLPEnabled is true — we never silently drop traces.
	OTLPEndpoint string
	// MetricsAddr exposes a Prometheus /metrics endpoint on the gateway
	// (and console). Empty (default) means no scrape surface, keeping
	// the zero-dep boot path free of goroutines.
	MetricsAddr string

	// Failover alert sinks (V4). Both are independently opt-in; an
	// empty URL disables the corresponding sink entirely (no
	// goroutines spun up, no DNS resolution attempted). Multiple sinks
	// can be active simultaneously — both are fanned out from the
	// gateway hot path via the same buffered async worker.
	FailoverWebhookURL string // generic JSON POST
	FailoverSlackURL   string // Slack-compatible incoming webhook
	// FailoverAlertCooldown coalesces back-to-back alerts onto the
	// same sink so a flapping primary doesn't melt an alert inbox.
	// Zero disables (each failover produces exactly one alert).
	FailoverAlertCooldown time.Duration

	// Metabase BI adapter (mirror of the V3 OTLP toggle). When MetabaseURL
	// is empty the adapter is fully off — NewMetabaseBootstrapper returns
	// nil and main.go skips boot wiring (no DNS / goroutines). When set, a
	// one-shot bootstrap on startup registers ClickHouse + Postgres as
	// Metabase datasources and seeds any collection JSONs shipped under
	// deploy/observability/metabase. Hot-path traces still go through the
	// existing recorder sinks (CH/OTLP); Metabase is pull-only.
	MetabaseURL            string
	MetabaseUser           string
	MetabasePassword       string
	MetabaseClickHouseURL  string // e.g. http://clickhouse:8123?database=nexus
	MetabasePostgresURL    string // e.g. postgres://nexus:nexus@postgres:5432/nexus
	MetabaseSeedDir        string // optional directory of collection JSONs
	MetabaseHealthTimeout  time.Duration
	MetabaseRequestTimeout time.Duration

	// Behavior
	UpstreamTimeout time.Duration

	// KeyMode controls how upstream provider keys are resolved per request:
	//   "shared" (default prior to v1) — use the process-wide env/org keys for everyone.
	//   "byok"             — each caller's request uses their own stored key,
	//                        falling back to org/env keys when they have none.
	//   "strict_byok"      — require a per-user key; reject calls from users who
	//                        have not registered a key for the target provider.
	//
	// As of v0.1.0 the default is "strict_byok" so the operator never pays for
	// user usage. To restore the legacy shared-key behavior, set
	// NEXUS_ALLOW_SHARED_KEYS=true (opt-in escape hatch — see AllowSharedKeys).
	KeyMode string

	// AllowSharedKeys is an opt-in escape hatch that re-enables env/orig-keys
	// as a soft fallback in any KeyMode. Defaults to false. When false, env
	// keys are still loaded for visibility (so an operator can see what is set)
	// but never reach the data path.
	AllowSharedKeys bool

	// Bootstrap admin: when set and no users exist yet, an admin account is
	// created on startup so the console has an initial login.
	AdminEmail    string
	AdminPassword string

	// AllowSignup enables public self-service registration at POST /api/auth/register.
	// New accounts are always created with the "member" role.
	AllowSignup bool

	// SSO (OIDC). When Enabled() returns true, Nexus exposes
	// /api/auth/sso/login and /api/auth/sso/callback and accepts a login
	// flow that exchanges an authorization code for a verified identity at
	// the configured issuer (Keycloak, Authentik, ...). Password login and
	// self-service signup stay available as fallbacks.
	SSO SSOConfig

	// Define the SSO + observability + identity knobs up here so the
	// struct is easy to scan; fields are grouped (auth, observability,
	// behavior, ...) in declaration order.

	// ReplicaID is a per-process identifier attached to every Trace. In a
	// multi-replica deployment set NEXUS_REPLICA_ID (or rely on the default
	// which is "hostname-randhex") so traces can be grouped by replica in
	// ClickHouse: `SELECT count() FROM gateway_traces GROUP BY replica_id`.
	// Stable for the lifetime of the process; rolling pods get a new id.
	ReplicaID string

	// DynamicModelSync periodically refreshes each provider's live model
	// list from its upstream /v1/models endpoint so /v1/models stays in
	// sync when providers add or sunset models without a Nexus redeploy.
	// Disabled by default — operators toggle it on with
	// NEXUS_DYNAMIC_MODEL_SYNC=true. Fetchers are skipped when a
	// provider's API key is absent, so leaving the flag on while only
	// some providers are configured is safe (those providers just don't
	// refresh).
	DynamicModelSync     bool
	DynamicModelInterval time.Duration // 0 → 30m default
	DynamicModelMaxRetry int           // 0 → 3 default
}

// SSOConfig is the OIDC configuration. The Enabled() predicate returns
// true only when all four required values are non-empty, so partial config
// (e.g. issuer set but client secret missing) safely degrades to "no SSO".
type SSOConfig struct {
	Issuer       string // e.g. https://keycloak.example/realms/cozy
	ClientID     string
	ClientSecret string
	RedirectURL  string // e.g. https://console.example/api/auth/sso/callback
	Label        string // UI button label, defaults to "SSO"
}

// Enabled reports whether the SSO flow is configured.
func (c SSOConfig) Enabled() bool {
	return c.Issuer != "" && c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

// LabelOrDefault returns the configured label, or "SSO" if unset.
func (c SSOConfig) LabelOrDefault() string {
	if c.Label == "" {
		return "SSO"
	}
	return c.Label
}

// Load reads configuration from the environment, applying sane defaults. It
// first loads a local .env file (if present) for developer convenience; real
// environment variables always take precedence over .env entries.
func Load() Config {
	loadDotEnv(".env")
	return Config{
		GatewayAddr:      env("NEXUS_GATEWAY_ADDR", ":8080"),
		ConsoleAddr:      env("NEXUS_CONSOLE_ADDR", ":8081"),
		PublicGatewayURL: env("NEXUS_PUBLIC_GATEWAY_URL", ""),
		PostgresURL:      env("NEXUS_POSTGRES_URL", ""),
		ClickHouseURL:    env("NEXUS_CLICKHOUSE_URL", ""),
		RedisURL:         env("NEXUS_REDIS_URL", ""),
		OpenAIAPIKey:     env("OPENAI_API_KEY", ""),
		OpenAIBaseURL:    env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		AnthropicAPIKey:  env("ANTHROPIC_API_KEY", ""),
		GeminiAPIKey:     env("GEMINI_API_KEY", ""),
		GroqAPIKey:       env("GROQ_API_KEY", ""),
		MistralAPIKey:    env("MISTRAL_API_KEY", ""),
		GridAPIKey:       env("GRID_API_KEY", ""),
		MasterKey:        env("NEXUS_MASTER_KEY", ""),
		EvalEnabled:      envBool("NEXUS_EVAL_ENABLED", true),
		JudgeBaseURL:     env("NEXUS_JUDGE_BASE_URL", ""),
		JudgeModel:       env("NEXUS_JUDGE_MODEL", "qwen2.5:7b"),
		JudgeAPIKey:      env("NEXUS_JUDGE_API_KEY", ""),
		EvalSampleRate:   envFloat("NEXUS_EVAL_SAMPLE_RATE", 1.0),
		EvalWorkers:      envInt("NEXUS_EVAL_WORKERS", 4),

		EvalServiceURL:         env("NEXUS_EVAL_SERVICE_URL", ""),
		EvalServiceMetrics:     env("NEXUS_EVAL_SERVICE_METRICS", "answer_relevancy,toxicity,bias"),
		EvalServiceTimeout:     envDuration("NEXUS_EVAL_SERVICE_TIMEOUT", 30*time.Second),
		RouteGroups:            env("NEXUS_ROUTE_GROUPS", ""),
		RouteWQuality:          envFloat("NEXUS_ROUTE_W_QUALITY", 0.6),
		RouteWCost:             envFloat("NEXUS_ROUTE_W_COST", 0.2),
		RouteWLatency:          envFloat("NEXUS_ROUTE_W_LATENCY", 0.2),
		RouteWindow:            envDuration("NEXUS_ROUTE_WINDOW", time.Hour),
		RouteRefresh:           envDuration("NEXUS_ROUTE_REFRESH", 30*time.Second),
		OTLPEnabled:            envBool("NEXUS_OTLP_ENABLED", false),
		OTLPEndpoint:           env("NEXUS_OTLP_ENDPOINT", ""),
		MetricsAddr:            env("NEXUS_METRICS_ADDR", ""),
		FailoverWebhookURL:     env("NEXUS_FAILOVER_WEBHOOK", ""),
		FailoverSlackURL:       env("NEXUS_FAILOVER_SLACK_WEBHOOK", ""),
		FailoverAlertCooldown:  envDuration("NEXUS_FAILOVER_ALERT_COOLDOWN", 0),
		MetabaseURL:            env("NEXUS_METABASE_URL", ""),
		MetabaseUser:           env("NEXUS_METABASE_USER", ""),
		MetabasePassword:       env("NEXUS_METABASE_PASSWORD", ""),
		MetabaseClickHouseURL:  env("NEXUS_METABASE_CLICKHOUSE_URL", ""),
		MetabasePostgresURL:    env("NEXUS_METABASE_POSTGRES_URL", ""),
		MetabaseSeedDir:        env("NEXUS_METABASE_SEED_DIR", ""),
		MetabaseHealthTimeout:  envDuration("NEXUS_METABASE_HEALTH_TIMEOUT", 90*time.Second),
		MetabaseRequestTimeout: envDuration("NEXUS_METABASE_REQUEST_TIMEOUT", 10*time.Second),
		UpstreamTimeout:        envDuration("NEXUS_UPSTREAM_TIMEOUT", 120*time.Second),
		KeyMode:                env("NEXUS_KEY_MODE", "strict_byok"),
		AllowSharedKeys:        envBool("NEXUS_ALLOW_SHARED_KEYS", false),
		AdminEmail:             env("NEXUS_ADMIN_EMAIL", ""),
		AdminPassword:          env("NEXUS_ADMIN_PASSWORD", ""),
		AllowSignup:            envBool("NEXUS_ALLOW_SIGNUP", false),

		SSO: SSOConfig{
			Issuer:       env("NEXUS_SSO_ISSUER", ""),
			ClientID:     env("NEXUS_SSO_CLIENT_ID", ""),
			ClientSecret: env("NEXUS_SSO_CLIENT_SECRET", ""),
			RedirectURL:  env("NEXUS_SSO_REDIRECT_URL", ""),
			Label:        env("NEXUS_SSO_LABEL", ""),
		},

		GuardrailsEnabled:     envBool("NEXUS_GUARDRAILS_ENABLED", false),
		GuardrailBlockPIIIn:   envBool("NEXUS_GUARDRAILS_BLOCK_PII_INPUT", false),
		GuardrailRedactPIIOut: envBool("NEXUS_GUARDRAILS_REDACT_PII_OUTPUT", false),
		GuardrailMaxInputChrs: envInt("NEXUS_GUARDRAILS_MAX_INPUT_CHARS", 0),
		GuardrailDenyPatterns: env("NEXUS_GUARDRAILS_DENY_PATTERNS", ""),
		GuardrailValidateJSON: envBool("NEXUS_GUARDRAILS_VALIDATE_JSON_OUTPUT", false),

		SelfCorrectionEnabled:    envBool("NEXUS_SELF_CORRECTION_ENABLED", false),
		SelfCorrectionMaxRetries: envInt("NEXUS_SELF_CORRECTION_MAX_RETRIES", 1),

		RouteLoadBalance:    envBool("NEXUS_ROUTE_LOAD_BALANCE", false),
		MaxConcurrentPerKey: envInt("NEXUS_MAX_CONCURRENT_PER_KEY", 0),

		SemanticCacheEnabled:    envBool("NEXUS_SEMANTIC_CACHE_ENABLED", false),
		SemanticCacheTTL:        envDuration("NEXUS_SEMANTIC_CACHE_TTL", 24*time.Hour),
		SemanticCacheThreshold:  envFloat("NEXUS_SEMANTIC_CACHE_THRESHOLD", 0.92),
		SemanticCacheMaxEntries: envInt("NEXUS_SEMANTIC_CACHE_MAX_ENTRIES", 500),
		EmbeddingsURL:           env("NEXUS_EMBEDDINGS_URL", ""),
		EmbeddingsModel:         env("NEXUS_EMBEDDINGS_MODEL", "text-embedding-3-small"),
		EmbeddingsAPIKey:        env("NEXUS_EMBEDDINGS_API_KEY", ""),
		EmbeddingsTimeout:       envDuration("NEXUS_EMBEDDINGS_TIMEOUT", 15*time.Second),

		ReplicaID: defaultReplicaID(),

		DynamicModelSync:     envBool("NEXUS_DYNAMIC_MODEL_SYNC", false),
		DynamicModelInterval: envDuration("NEXUS_DYNAMIC_MODEL_INTERVAL", 30*time.Minute),
		DynamicModelMaxRetry: envInt("NEXUS_DYNAMIC_MODEL_MAX_RETRY", 3),
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

// defaultReplicaID builds a stable id for this process. Operators can pin it
// via NEXUS_REPLICA_ID; otherwise we fall back to "<hostname>-<randid>" so
// traces from a rolling deployment are still distinguishable in
// ClickHouse. The hostname piece is informational; the randid guarantees
// uniqueness across processes even on the same host.
func defaultReplicaID() string {
	if explicit := strings.TrimSpace(env("NEXUS_REPLICA_ID", "")); explicit != "" {
		return explicit
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "nexus"
	}
	if idx := strings.IndexByte(host, '.'); idx > 0 {
		host = host[:idx] // trim FQDN to bare pod host name in k8s
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// rand failure is vanishingly rare; degrade gracefully.
		return host + "-node"
	}
	return host + "-" + hex.EncodeToString(buf[:])
}
