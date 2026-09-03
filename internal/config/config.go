// Package config loads runtime configuration from environment variables.
package config

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"
)

// emailFromAddrVarNames is the canonical order for resolving the From
// address. New names come first so an operator who sets both wins on the
// new var. Kept package-level so a test in this package can drive the
// resolution directly with os.Setenv calls.
var emailFromAddrVarNames = []string{"NEXUS_EMAIL_FROM_ADDRESS", "NEXUS_RESEND_FROM_ADDRESS"}

// Config holds all runtime configuration for the Nexus gateway.
type Config struct {
	// HTTP
	GatewayAddr      string   // gateway proxy listen address
	ConsoleAddr      string   // console API / dashboard listen address
	PublicGatewayURL string   // user-facing base URL shown in the console (optional)
	PublicBaseURL    string   // user-facing base URL for vendor-side webhook URLs (optional)
	PublicWebOrigins []string // operator-supplied allow-list of web origins
	//                     that the console may connect to. Wired into
	//                     the CSP `connect-src` directive so the policy
	//                     no longer hardcodes any company's domain.

	// EgressTenantAllowedCIDRs re-permits specific address ranges for outbound
	// requests whose destination an org admin configured — an eval profile's
	// base_url, a plugin manifest endpoint, a credential base_url.
	//
	// Those destinations are refused from private, loopback and link-local
	// ranges by default, because the pod's network position is a privilege the
	// API caller does not otherwise have: the same request would otherwise reach
	// the cloud metadata service, and two of these paths persist the response
	// where the caller can read it.
	//
	// The escape hatch exists for the real case of a customer running Langfuse
	// or an OTLP collector inside the cluster and wanting org admins to point
	// plugins at it. It is a CIDR list rather than a boolean so that widening the
	// policy is a specific decision. It can never re-permit link-local, which is
	// where instance metadata lives.
	//
	// NEXUS_EGRESS_TENANT_ALLOWED_CIDRS, e.g. "10.44.0.0/16,10.45.1.7".
	EgressTenantAllowedCIDRs string
	// PublicGrafanaURL is the browser-reachable base URL of the
	// operator's OWN Grafana. It is used for one thing only: composing
	// the deep links that GET /api/ui/observability hands to the
	// console so it can render an "Open in Grafana" affordance. Nexus
	// never calls Grafana server-side, so an unreachable or wrong value
	// costs a broken hyperlink and nothing else — in particular it
	// cannot affect the gateway's request path.
	//
	// Empty (the default) makes /api/ui/observability report no Grafana,
	// and the console omits the link.
	PublicGrafanaURL string
	// DocsDir is the on-disk path the console serves documentation
	// from. Empty falls back to ./docs relative to the binary, which
	// matches a `go run ./cmd/nexus` invocation; production
	// deployments typically mount the repo's /docs sub-tree.
	DocsDir string
	//                     Comma-separated env (NEXUS_PUBLIC_WEB_ORIGINS);
	//                     empty == CSP falls back to 'self' only, which
	//                     is the safe default for on-prem Helm deploys.

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
	PluginDir          string // directory containing Helm-mounted EvalPlugin YAMLs

	// EvalPluginOnly when true suppresses the seed of built-in
	// heuristic profiles (PII / Completeness) at boot. Operators
	// who want *only* external plugin-driven eval (Langfuse,
	// LangSmith, DeepEval cloud, etc.) flip this on. It is not
	// destructive: existing rows in the profile store are left
	// alone — the flag controls what gets inserted on an empty
	// store, not what already exists. Operators must delete the
	// existing built-in rows through the console if they want a
	// pure plugin set immediately after upgrade.
	EvalPluginOnly bool

	// PurgeLegacyProfilesOnBoot is the *destructive* companion to
	// EvalPluginOnly. When both are true the runtime controller
	// hard-deletes the well-known seed rows
	//   default-pii, default-completeness, default-judge,
	//   default-remote
	// from the profile store during the SeedProfilesFromConfig
	// pass on each boot. Operators adopt this when they have
	// external PII / completeness coverage from a vendor plugin
	// and want the cluster to converge on a plugin-only profile
	// set without manual cleanup. The gate is intentionally two
	// flags because we want an extra deliberate operator step
	// before turning deletion on — lost rows are not recoverable
	// from this code path.
	PurgeLegacyProfilesOnBoot bool

	// Quality-aware routing (Phase 4). RouteGroups maps an alias to candidate
	// models, e.g. "fast=gpt-4o-mini,gemini-2.5-flash;smart=gpt-4o,claude-...".
	// The built-in alias "auto" always routes across all registered models.
	RouteGroups   string
	RouteWQuality float64

	// RouteWBench controls how PrimeIntellect / external benchmark
	// scores mix with rolling judge scores. 0 disables the bench
	// blend entirely; 1 makes a fresh benchmark the dominant
	// signal (judge is ignored). 0.5 is the default.
	//
	// Stale benchmark influence decays via RouteBenchHalfLife:
	// a 14-day-old run has half the weight it had at completion.
	// benchmark_runs lives in Postgres today; the blend path is
	// a no-op when Postgres is not configured.
	RouteWBench        float64
	RouteBenchHalfLife time.Duration

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

	// CaptureTraceContent controls whether prompts, completions, and
	// retrieval contexts are written to durable trace storage. Default
	// false: the gateway still carries them in memory, because the
	// evaluators cannot score a trace whose bodies are blank, but
	// observability.CaptureGate strips them before the ClickHouse
	// recorder sees them. Set true to retain bodies for the console
	// trace inspector and for replay.
	//
	// Default-off is the deliberate choice, not a conservative
	// placeholder: a customer who never edits a value file should not
	// find ninety days of their users' prompts in a table they did not
	// know had those columns. Operators who want the inspector opt in
	// and, in doing so, make retention a decision someone recorded.
	CaptureTraceContent bool

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

	// Invitation email transport.
	//
	// Invites are persisted to invite_tokens in all modes — the admin can
	// always copy the URL out of the console. This block just decides whether
	// a transactional envelope is also sent, and through which transport.
	//
	// The transport is the customer's own mail infrastructure. A self-hosted
	// install must send from a domain the customer controls and has SPF/DKIM
	// signed for; Nexus does not ship a vendor mail account, and there is no
	// default `From` so a half-configured install cannot accidentally issue
	// mail from a domain the customer cannot authenticate.
	//
	// Provider resolution (when NEXUS_EMAIL_PROVIDER is empty):
	//   - NEXUS_RESEND_API_KEY  set → resend
	//   - NEXUS_SMTP_HOST       set → smtp
	//   - neither               → disabled (copyable URL only, no fault)
	// With NEXUS_EMAIL_PROVIDER set explicitly, the value must be one of
	// "resend", "smtp" or "noop" — and "noop" is only valid under
	// NEXUS_DEV_MODE, which the chart deliberately does not expose, so a
	// customer install cannot select it.
	//
	// Legacy NEXUS_RESEND_FROM_ADDRESS / NEXUS_RESEND_REQUEST_TIMEOUT
	// remain readable as aliases so a user who already deployed Resend
	// changes nothing but the `From` value; the engine logs a warn line
	// when an alias supplies the address, so the migration is visible in
	// pod logs.
	EmailProvider       string
	EmailFromAddress    string
	EmailRequestTimeout time.Duration
	ResendAPIKey        string
	ResendAPIBaseURL    string
	SMTPHost            string
	SMTPPort            int
	SMTPUsername        string
	SMTPPassword        string
	SMTPEncryption      string
	// EmailPublicBaseURL overrides the invite URL embedded in the
	// outgoing email when the operator wants a different host on the
	// envelope than the console URL the invitee clicks through.
	// Empty falls back to PublicBaseURL.
	EmailPublicBaseURL string

	// AutoMigrate applies outstanding schema migrations during boot.
	//
	// Default FALSE, which is a deliberate change from the original behaviour
	// of migrating on every boot of every replica. In a customer deployment the
	// schema is advanced by a separate `nexus migrate` step (the Helm
	// pre-install/pre-upgrade hook Job) so that a migration failure aborts the
	// release instead of being discovered from user-visible errors after the
	// rollout. When false and migrations are outstanding, the process starts
	// but reports NotReady on /readyz with the list of pending migrations, so
	// it never serves traffic against a schema it does not match.
	//
	// Set true for local development (docker compose), where "bring up an empty
	// database and just work" is worth more than deployment gating.
	AutoMigrate bool

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

	// DevMode relaxes browser-security defaults that are impossible to satisfy
	// over plain HTTP on localhost: cookies stop being Secure-only, and
	// http://localhost / http://127.0.0.1 origins are accepted.
	//
	// It must never be set in a customer deployment. Nothing about it is
	// convenient in production - it only removes protections - so it is a single
	// obvious flag rather than a scattering of individual overrides, and main.go
	// logs a warning at boot when it is on.
	DevMode bool

	// SecureCookies forces the Secure attribute on session and OIDC state
	// cookies. Defaults to true, and to false when DevMode is set.
	//
	// Session cookies previously carried no Secure attribute at all, so a
	// browser would send a live console session over plain HTTP - to a
	// downgraded link, or to an attacker who can strip TLS. The console is
	// always behind TLS in a real deployment, so Secure is correct by default
	// and the flag exists for the plain-HTTP local case.
	//
	// Nexus normally terminates TLS at an ingress and receives plain HTTP, so
	// this cannot be inferred from the request - hence configuration rather
	// than detection.
	SecureCookies bool

	// PublicDocs serves /api/docs/* without a session. Default false.
	//
	// The docs tree was previously unauthenticated by accident: a comment
	// claimed a blanket requireUser middleware protected it, no such middleware
	// existed, and so the bundle went out to anyone who could reach the port.
	// None of it is secret, but it enumerates this installation's endpoints,
	// configuration flags and operational procedures, which for a
	// single-customer deployment is internal documentation. Authenticated by
	// default; a public docs site is an explicit choice.
	PublicDocs bool

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

// EmailMode constants. The closed enumerate maps to the closed set of
// transport implementations in console.Mailer. Boot prefers an explicit
// NEXUS_EMAIL_PROVIDER; an empty value auto-picks based on which
// transport field is populated.
const (
	EmailModeSMTP   = "smtp"
	EmailModeResend = "resend"
	// EmailModeNoop is the in-process discard transport. Boot refuses
	// to pick it unless DevMode is true; otherwise an unconfigured cluster
	// boots and silently drops invites.
	EmailModeNoop = "noop"
)

// EmailMode reports the resolved transport and the address that should appear
// in the From: header. Operators pick at install time; the binary refuses to
// boot when a transport is named but no address is provided — sending from a
// domain the customer cannot authenticate for is worse than not sending.
//
// The set of named providers is closed. An empty mode means "no transport";
// invites still flow through the copyable URL path, so the absence is a
// feature, not a fault.
//
// Validate returns ErrEmailMisconfigured when the operator named a provider
// but left the From unset (or unparseable, or a CRLF carrier); ErrEmailUnknown
// when the named provider is not one of the three. devOnly == true is set
// when the resolved mode is "noop" so the boot path can refuse it without
// NEXUS_DEV_MODE.
func (c Config) EmailMode() (mode, from string, devOnly bool, err error) {
	switch strings.ToLower(strings.TrimSpace(c.EmailProvider)) {
	case EmailModeResend:
		mode = EmailModeResend
		if c.ResendAPIKey == "" {
			return "", "", false, errors.New("config: provider=resend requires NEXUS_RESEND_API_KEY")
		}
	case EmailModeSMTP:
		mode = EmailModeSMTP
		if strings.TrimSpace(c.SMTPHost) == "" {
			return "", "", false, errors.New("config: provider=smtp requires NEXUS_SMTP_HOST")
		}
	case EmailModeNoop:
		return EmailModeNoop, "", true, nil
	case "":
		// Auto-detect. Resend wins on key presence; SMTP wins on host
		// presence; otherwise no transport.
		switch {
		case strings.TrimSpace(c.ResendAPIKey) != "":
			mode = EmailModeResend
		case strings.TrimSpace(c.SMTPHost) != "":
			mode = EmailModeSMTP
		default:
			return "", "", false, nil
		}
	default:
		return "", "", false, fmt.Errorf("config: unknown NEXUS_EMAIL_PROVIDER %q (want resend, smtp, noop)", c.EmailProvider)
	}
	from = strings.TrimSpace(c.EmailFromAddress)
	if from == "" {
		return "", "", false, errors.New("config: a transport is configured but NEXUS_EMAIL_FROM_ADDRESS is empty; refusing to send mail from an unauthenticated domain")
	}
	if _, perr := mail.ParseAddress(from); perr != nil {
		return "", "", false, fmt.Errorf("config: NEXUS_EMAIL_FROM_ADDRESS %q is not a valid RFC 5322 address: %w", c.EmailFromAddress, perr)
	}
	return mode, from, false, nil
}

// EmailEnabled is the splash-of-true probe used by handlers that speak
// console-side ("is there a Mailer wired?"). Returns false when no transport
// is configured, when DevMode-only noop is selected and DevMode is off, and
// when From is missing — boot will have already terminated the last two, but
// the handler still needs to answer politely so a Send call does not nil-out.
func (c Config) EmailEnabled() bool {
	mode, from, devOnly, err := c.EmailMode()
	if err != nil || from == "" {
		return false
	}
	if devOnly {
		return c.DevMode
	}
	return mode != ""
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
	c := load()

	// SecureCookies defaults to on, and follows DevMode down rather than being
	// a second thing to remember. An operator can still force it back on in dev
	// (NEXUS_SECURE_COOKIES=true) if they run local TLS.
	c.SecureCookies = envBool("NEXUS_SECURE_COOKIES", !c.DevMode)

	return c
}

func load() Config {
	emailFromAddress := firstNonEmpty(
		env("NEXUS_EMAIL_FROM_ADDRESS", ""),
		env("NEXUS_RESEND_FROM_ADDRESS", ""),
	)
	emailTimeout := firstDurNonZero(
		envDuration("NEXUS_EMAIL_REQUEST_TIMEOUT", 10*time.Second),
		envDuration("NEXUS_RESEND_REQUEST_TIMEOUT", 0),
	)
	return Config{
		GatewayAddr:      env("NEXUS_GATEWAY_ADDR", ":8080"),
		ConsoleAddr:      env("NEXUS_CONSOLE_ADDR", ":8081"),
		PublicGatewayURL: env("NEXUS_PUBLIC_GATEWAY_URL", ""),
		PublicBaseURL:    env("NEXUS_PUBLIC_BASE_URL", ""),
		PublicWebOrigins: splitCSV(env("NEXUS_PUBLIC_WEB_ORIGINS", "")),

		EgressTenantAllowedCIDRs: env("NEXUS_EGRESS_TENANT_ALLOWED_CIDRS", ""),
		PublicGrafanaURL:         env("NEXUS_PUBLIC_GRAFANA_URL", ""),
		DocsDir:                  env("NEXUS_DOCS_DIR", ""),
		PostgresURL:              env("NEXUS_POSTGRES_URL", ""),
		ClickHouseURL:            env("NEXUS_CLICKHOUSE_URL", ""),
		RedisURL:                 env("NEXUS_REDIS_URL", ""),
		OpenAIAPIKey:             env("OPENAI_API_KEY", ""),
		OpenAIBaseURL:            env("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		AnthropicAPIKey:          env("ANTHROPIC_API_KEY", ""),
		GeminiAPIKey:             env("GEMINI_API_KEY", ""),
		GroqAPIKey:               env("GROQ_API_KEY", ""),
		MistralAPIKey:            env("MISTRAL_API_KEY", ""),
		GridAPIKey:               env("GRID_API_KEY", ""),
		MasterKey:                env("NEXUS_MASTER_KEY", ""),
		EvalEnabled:              envBool("NEXUS_EVAL_ENABLED", true),
		JudgeBaseURL:             env("NEXUS_JUDGE_BASE_URL", ""),
		JudgeModel:               env("NEXUS_JUDGE_MODEL", "qwen2.5:7b"),
		JudgeAPIKey:              env("NEXUS_JUDGE_API_KEY", ""),
		EvalSampleRate:           envFloat("NEXUS_EVAL_SAMPLE_RATE", 1.0),
		EvalWorkers:              envInt("NEXUS_EVAL_WORKERS", 4),

		EvalServiceURL:            env("NEXUS_EVAL_SERVICE_URL", ""),
		EvalServiceMetrics:        env("NEXUS_EVAL_SERVICE_METRICS", "answer_relevancy,toxicity,bias"),
		EvalServiceTimeout:        envDuration("NEXUS_EVAL_SERVICE_TIMEOUT", 30*time.Second),
		PluginDir:                 env("NEXUS_EVAL_PLUGIN_DIR", "/etc/nexus/eval-plugins"),
		EvalPluginOnly:            envBool("NEXUS_EVAL_PLUGIN_ONLY", false),
		PurgeLegacyProfilesOnBoot: envBool("NEXUS_EVAL_PURGE_LEGACY_PROFILES_ON_BOOT", false),
		RouteGroups:               env("NEXUS_ROUTE_GROUPS", ""),
		RouteWQuality:             envFloat("NEXUS_ROUTE_W_QUALITY", 0.6),
		RouteWCost:                envFloat("NEXUS_ROUTE_W_COST", 0.2),
		RouteWLatency:             envFloat("NEXUS_ROUTE_W_LATENCY", 0.2),
		RouteWindow:               envDuration("NEXUS_ROUTE_WINDOW", time.Hour),
		RouteRefresh:              envDuration("NEXUS_ROUTE_REFRESH", 30*time.Second),
		RouteWBench:               envFloat("NEXUS_ROUTE_W_BENCH", 0.5),
		RouteBenchHalfLife:        envDuration("NEXUS_ROUTE_BENCH_HALF_LIFE", 7*24*time.Hour),
		OTLPEnabled:               envBool("NEXUS_OTLP_ENABLED", false),
		OTLPEndpoint:              env("NEXUS_OTLP_ENDPOINT", ""),
		MetricsAddr:               env("NEXUS_METRICS_ADDR", ""),
		CaptureTraceContent:       envBool("NEXUS_CAPTURE_TRACE_CONTENT", false),
		FailoverWebhookURL:        env("NEXUS_FAILOVER_WEBHOOK", ""),
		FailoverSlackURL:          env("NEXUS_FAILOVER_SLACK_WEBHOOK", ""),
		FailoverAlertCooldown:     envDuration("NEXUS_FAILOVER_ALERT_COOLDOWN", 0),
		MetabaseURL:               env("NEXUS_METABASE_URL", ""),
		MetabaseUser:              env("NEXUS_METABASE_USER", ""),
		MetabasePassword:          env("NEXUS_METABASE_PASSWORD", ""),
		MetabaseClickHouseURL:     env("NEXUS_METABASE_CLICKHOUSE_URL", ""),
		MetabasePostgresURL:       env("NEXUS_METABASE_POSTGRES_URL", ""),
		MetabaseSeedDir:           env("NEXUS_METABASE_SEED_DIR", ""),
		MetabaseHealthTimeout:     envDuration("NEXUS_METABASE_HEALTH_TIMEOUT", 90*time.Second),
		MetabaseRequestTimeout:    envDuration("NEXUS_METABASE_REQUEST_TIMEOUT", 10*time.Second),

		// EmailProvider is the resolved value above (new or alias);
		// the timeout uses the resolved value above; the request timeout
		// stays tied to EmailRequestTimeout for symmetry with the prior
		// ResendRequestTimeout, since either transport should honour it.
		EmailProvider:       env("NEXUS_EMAIL_PROVIDER", ""),
		EmailFromAddress:    emailFromAddress,
		EmailRequestTimeout: emailTimeout,
		// Resend-specific. The base URL exists so an air-gapped install
		// can point at an internal relay that mimics the Resend contract.
		ResendAPIKey:     env("NEXUS_RESEND_API_KEY", ""),
		ResendAPIBaseURL: env("NEXUS_RESEND_API_BASE_URL", ""),
		// SMTP-specific. Password goes in NEXUS_SMTP_PASSWORD, rendered
		// into the Secret by the chart.
		SMTPHost:       env("NEXUS_SMTP_HOST", ""),
		SMTPPort:       envInt("NEXUS_SMTP_PORT", 587),
		SMTPUsername:   env("NEXUS_SMTP_USERNAME", ""),
		SMTPPassword:   env("NEXUS_SMTP_PASSWORD", ""),
		SMTPEncryption: env("NEXUS_SMTP_ENCRYPTION", "starttls"),

		EmailPublicBaseURL: env("NEXUS_EMAIL_PUBLIC_BASE_URL", ""),
		AutoMigrate:        envBool("NEXUS_AUTO_MIGRATE", false),
		UpstreamTimeout:    envDuration("NEXUS_UPSTREAM_TIMEOUT", 120*time.Second),
		KeyMode:            env("NEXUS_KEY_MODE", "strict_byok"),
		AllowSharedKeys:    envBool("NEXUS_ALLOW_SHARED_KEYS", false),
		AdminEmail:         env("NEXUS_ADMIN_EMAIL", ""),
		AdminPassword:      env("NEXUS_ADMIN_PASSWORD", ""),
		AllowSignup:        envBool("NEXUS_ALLOW_SIGNUP", false),
		PublicDocs:         envBool("NEXUS_PUBLIC_DOCS", false),
		DevMode:            envBool("NEXUS_DEV_MODE", false),

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

// splitCSV turns a comma-separated env string into a clean slice, trimming
// whitespace and dropping empties. Used by Config fields whose underlying
// env var is documented as a CSV (e.g. NEXUS_PUBLIC_WEB_ORIGINS).
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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

// firstNonEmpty returns the first string that, after trim, is non-empty.
// Used to read the new email env var first and fall back to a deprecated
// alias without duplicating the env-reading call millions of times.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstDurNonZero returns the first non-zero duration. A zero duration here
// means "unset" rather than "we want zero-timeouts"; callers asking the env
// for NEXUS_RESEND_REQUEST_TIMEOUT and getting back a Go zero are reading
// the deprecated alias path.
func firstDurNonZero(values ...time.Duration) time.Duration {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
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
