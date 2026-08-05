// Package benchmark drives model-level quality measurements on an
// external eval platform. PrimeIntellect hosted evaluations are the
// only provider today.
//
// This sits beside the eval plugin system rather than inside it. A
// plugin answers "how good was this trace", so its input is a trace
// and the dispatcher fires per request. A benchmark answers "how good
// is this model", so its input is a model plus a dataset, it runs for
// minutes to hours, and there is no trace to hang the result on. The
// two share credentials and org scoping and nothing else.
//
// Nexus never runs the harness. The vendor loads the dataset, drives
// the model and applies the verifier inside their own sandbox; we hold
// the run record and poll for the aggregate. The one thing we do
// contribute is the inference endpoint: a run can be pointed back at
// this gateway so the score describes the model as we actually serve
// it, with routing, cache and provider selection included.
package benchmark

import (
	"errors"
	"fmt"
	"time"
)

// Provider names the external platform executing a run. Stored on the
// row so a second provider can be added without reinterpreting
// existing history.
const ProviderPrime = "primeintellect"

// PrimeAPIBase is the documented API host. Kept as a constant rather
// than a config knob because the vendor publishes no regional
// variants — unlike Langfuse, where assuming one host cost us a day.
const PrimeAPIBase = "https://api.primeintellect.ai"

// CredentialName and CredentialKey locate the provider token inside the
// existing encrypted plugin-key store.
//
// Reusing that table rather than adding one keeps every vendor secret on
// the same master-key cipher and the same durability guarantee. The name
// is reserved: it cannot collide with a plugin because no EvalPlugin
// manifest is allowed to claim it.
const (
	CredentialName      = "benchmark-primeintellect"
	CredentialKey       = "api_key"
	CredentialTeamIDKey = "team_id"
)

// Run lifecycle. Deliberately coarser than the vendor's state machine:
// callers branch on these, and the vendor's raw string is preserved
// alongside so no detail is lost in the collapse.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// MaxTotalSamples caps num_examples × rollouts for a single run.
//
// A hosted run bills per token and may stay alive for up to 24 hours,
// so an unbounded console form is a way to spend real money with one
// click. The cap is intentionally low enough that hitting it prompts a
// conversation rather than a refund request; raise it deliberately.
const MaxTotalSamples = 2000

// Vendor status strings, from EvaluationStatus in the Prime OpenAPI
// document. Listed exhaustively so an unrecognised value is treated as
// unsettled rather than silently read as success.
const (
	primeStatusPending    = "PENDING"
	primeStatusRunning    = "RUNNING"
	primeStatusProcessing = "PROCESSING"
	primeStatusCompleted  = "COMPLETED"
	primeStatusFailed     = "FAILED"
	primeStatusTimeout    = "TIMEOUT"
	primeStatusCancelled  = "CANCELLED"
)

// Error classes. Callers at the HTTP boundary map these onto status
// codes, so they exist as sentinels rather than as prose an handler
// would have to pattern-match. Each wrapped error keeps its own message,
// which is where the offending value lives.
var (
	// ErrNoToken reports a launch or poll attempted with no vendor
	// credential resolved.
	ErrNoToken = errors.New("benchmark: no provider API token configured")
	// ErrInvalidRequest marks input the caller can correct: a missing
	// field, a value out of range, or a run over the cost cap.
	ErrInvalidRequest = errors.New("benchmark: invalid request")
	// ErrConflict marks an operation refused because of the run's
	// current state, such as cancelling one that already settled.
	ErrConflict = errors.New("benchmark: conflicting run state")
)

// LaunchRequest is the Nexus-shaped description of a run. The wire
// encoding lives in client.go so this stays readable at call sites.
type LaunchRequest struct {
	// Environments are vendor Hub slugs such as "primeintellect/gsm8k".
	// The hosted-evaluations create endpoint expects internal ids;
	// Client.resolveEnvironmentIDs maps slugs before create.
	Environments []string
	// Model is passed through as the vendor's inference_model. When
	// BaseURL is set the vendor forwards it as the OpenAI "model"
	// field, so it must name a model this gateway will accept.
	Model       string
	NumExamples int
	Rollouts    int
	Name        string

	// BaseURL, KeyVar and Secrets route the vendor's inference back
	// through Nexus. BaseURL is our /v1 endpoint, KeyVar names the
	// environment variable the sandbox reads the key from, and
	// Secrets carries that key. All three are set together or none
	// are: a BaseURL with no key produces a run that fails on its
	// first request.
	BaseURL string
	KeyVar  string
	Secrets map[string]string

	// TimeoutMinutes is the vendor's run deadline. Zero omits the
	// field and accepts their default of 24 hours. The vendor
	// rejects anything below 120.
	TimeoutMinutes int

	// TeamID bills the run against a Prime team wallet instead of
	// the personal wallet tied to the API key. Empty omits the
	// field and the vendor defaults to personal billing.
	TeamID string
}

// TotalSamples is the unit the cap and the bill are both measured in.
func (r LaunchRequest) TotalSamples() int { return r.NumExamples * r.Rollouts }

// Validate rejects requests the vendor would reject anyway, plus the
// cost cap, which is ours. Failing here keeps a bad form submission
// from becoming a vendor round-trip and a half-written row.
func (r LaunchRequest) Validate() error {
	if len(r.Environments) == 0 {
		return fmt.Errorf("%w: at least one environment is required", ErrInvalidRequest)
	}
	for _, e := range r.Environments {
		if e == "" {
			return fmt.Errorf("%w: environment slug must not be empty", ErrInvalidRequest)
		}
	}
	if r.Model == "" {
		return fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	// -1 is the vendor's "every example" sentinel. We refuse it
	// rather than translate it, because an unbounded run cannot be
	// checked against the cost cap.
	if r.NumExamples < 1 {
		return fmt.Errorf("%w: num_examples must be at least 1", ErrInvalidRequest)
	}
	if r.Rollouts < 1 {
		return fmt.Errorf("%w: rollouts must be at least 1", ErrInvalidRequest)
	}
	if n := r.TotalSamples(); n > MaxTotalSamples {
		return fmt.Errorf("%w: %d total samples (%d examples × %d rollouts) exceeds the %d cap",
			ErrInvalidRequest, n, r.NumExamples, r.Rollouts, MaxTotalSamples)
	}
	if r.TimeoutMinutes != 0 && (r.TimeoutMinutes < 120 || r.TimeoutMinutes > 1440) {
		return fmt.Errorf("%w: timeout_minutes must be between 120 and 1440 when set", ErrInvalidRequest)
	}
	if r.BaseURL != "" {
		if r.KeyVar == "" {
			return fmt.Errorf("%w: gateway-routed runs need an api key variable name", ErrInvalidRequest)
		}
		if r.Secrets[r.KeyVar] == "" {
			return fmt.Errorf("%w: gateway-routed runs need a secret for %q", ErrInvalidRequest, r.KeyVar)
		}
	}
	return nil
}

// DryRunResult is the outcome of a pre-flight probe. Warning is set
// when slug and credential passed but the vendor signalled a billing
// block — launch may still fail until the wallet is funded.
type DryRunResult struct {
	Warning string
}

// LaunchResult is what the vendor returns from a create call. A run
// spanning several environments yields several ids; we keep the first
// as the row's external id and record the rest for reference.
type LaunchResult struct {
	EvaluationID  string   `json:"evaluation_id"`
	SandboxID     string   `json:"sandbox_id"`
	Status        string   `json:"status"`
	EvaluationIDs []string `json:"evaluation_ids"`
	Error         string   `json:"error"`
}

// Evaluation is the vendor's view of one run, including the aggregate
// once it settles. Scores are pointers because "not scored yet" and
// "scored zero" are different answers and a plain float cannot tell
// them apart.
type Evaluation struct {
	EvaluationID     string         `json:"evaluation_id"`
	Name             string         `json:"name"`
	Status           string         `json:"status"`
	IsHosted         bool           `json:"is_hosted"`
	InferenceModel   string         `json:"inference_model"`
	EnvironmentNames []string       `json:"environment_names"`
	TotalSamples     int            `json:"total_samples"`
	AvgScore         *float64       `json:"avg_score"`
	MinScore         *float64       `json:"min_score"`
	MaxScore         *float64       `json:"max_score"`
	Metrics          map[string]any `json:"metrics"`
	ViewerURL        string         `json:"viewer_url"`
	ErrorMessage     string         `json:"error_message"`
	StartedAt        *time.Time     `json:"started_at"`
	CompletedAt      *time.Time     `json:"completed_at"`
}

// Model is one entry from the vendor's inference catalogue. Pricing is
// surfaced so the console can warn before an expensive run rather
// than after one.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Pricing  struct {
		Prompt     float64 `json:"prompt"`
		Completion float64 `json:"completion"`
	} `json:"pricing"`
}

// NormalizeStatus collapses a vendor status onto our lifecycle.
//
// An unknown string maps to running, not failed. The vendor can add
// states, and treating a new intermediate state as terminal would
// abandon a run that is still progressing and bill for a result we
// never read.
func NormalizeStatus(vendor string) string {
	switch vendor {
	case primeStatusPending:
		return StatusPending
	case primeStatusCompleted:
		return StatusCompleted
	case primeStatusFailed, primeStatusTimeout:
		return StatusFailed
	case primeStatusCancelled:
		return StatusCancelled
	case primeStatusRunning, primeStatusProcessing:
		return StatusRunning
	default:
		return StatusRunning
	}
}

// Settled reports whether one of our statuses needs no further polling.
func Settled(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}
