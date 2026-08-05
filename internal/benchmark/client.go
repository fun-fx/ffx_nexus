package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one provider account. The token is held per client
// rather than passed per call because it is resolved once from the key
// vault and every method needs it; constructing a client per operation
// is cheap and keeps the credential's lifetime obvious.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient builds a client for the given account. An empty base falls
// back to the documented host so callers only override it in tests.
func NewClient(base, token string, hc *http.Client) *Client {
	if base == "" {
		base = PrimeAPIBase
	}
	if hc == nil {
		// Generous but finite: the launch call provisions a sandbox
		// before answering, so it is slower than a plain API write.
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, hc: hc}
}

// wire shapes. Kept unexported so the vendor's field names stay
// confined to this file and LaunchRequest can read naturally.
type createRequest struct {
	EnvironmentIDs []string   `json:"environment_ids"`
	InferenceModel string     `json:"inference_model"`
	EvalConfig     evalConfig `json:"eval_config"`
	Name           string     `json:"name,omitempty"`
	TeamID         string     `json:"team_id,omitempty"`
}

type evalConfig struct {
	NumExamples        int               `json:"num_examples"`
	RolloutsPerExample int               `json:"rollouts_per_example"`
	TimeoutMinutes     *int              `json:"timeout_minutes,omitempty"`
	APIBaseURL         string            `json:"api_base_url,omitempty"`
	APIKeyVar          string            `json:"api_key_var,omitempty"`
	CustomSecrets      map[string]string `json:"custom_secrets,omitempty"`
}

// Launch starts a hosted run and returns the vendor's identifiers.
//
// The request is validated first so a bad form never reaches the
// vendor, and never leaves a row claiming to track a run that was
// refused.
func (c *Client) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	if c == nil || c.token == "" {
		return LaunchResult{}, ErrNoToken
	}
	if err := req.Validate(); err != nil {
		return LaunchResult{}, err
	}
	envIDs, err := c.resolveEnvironmentIDs(ctx, req.Environments)
	if err != nil {
		return LaunchResult{}, err
	}
	body := createRequest{
		EnvironmentIDs: envIDs,
		InferenceModel: req.Model,
		Name:           req.Name,
		TeamID:         strings.TrimSpace(req.TeamID),
		EvalConfig: evalConfig{
			NumExamples:        req.NumExamples,
			RolloutsPerExample: req.Rollouts,
			APIBaseURL:         req.BaseURL,
			APIKeyVar:          req.KeyVar,
			CustomSecrets:      req.Secrets,
		},
	}
	if req.TimeoutMinutes > 0 {
		body.EvalConfig.TimeoutMinutes = &req.TimeoutMinutes
	}
	var out LaunchResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/hosted-evaluations", body, &out); err != nil {
		return LaunchResult{}, err
	}
	if out.EvaluationID == "" {
		// The vendor documents evaluation_id as required, so an empty
		// one means we cannot poll. Surface their error text if any,
		// since a run may have started that we can no longer track.
		if out.Error != "" {
			return out, fmt.Errorf("benchmark: launch returned no evaluation id: %s", out.Error)
		}
		return out, errors.New("benchmark: launch returned no evaluation id")
	}
	return out, nil
}

// DryRun verifies the credential and the environment slugs against
// the vendor without producing a billable run.
//
// The cheapest known probe is a hosted-evaluations create with
// NumExamples=1 followed by an immediate cancel. The cancel call
// returns before the sandbox is provisioned, so the run never executes
// the dataset and consumes no model tokens. A 404 from create means
// the slug is not visible to this account; a 401 means the key was
// rejected; otherwise the run exists for the lifetime of the cancel
// round-trip and is gone by the time the function returns.
//
// The vendor's create endpoint treats NumExamples=1 as a single-row
// sandbox; we pass Rollouts=1 for the same reason. The request body
// is the same wire shape the regular Launch uses so a regression in
// the encoded fields fails here just as loudly.
//
// We deliberately do not write a benchmark_runs row for a DryRun:
// leaving one even briefly makes the poller queue it and the operator
// sees a transient failed row in the console. The whole point of the
// probe is to answer a question; no durable evidence is needed.
func (c *Client) DryRun(ctx context.Context, req LaunchRequest) (DryRunResult, error) {
	if c == nil || c.token == "" {
		return DryRunResult{}, ErrNoToken
	}
	probe := LaunchRequest{
		Environments:   req.Environments,
		Model:          req.Model,
		NumExamples:    1,
		Rollouts:       1,
		Name:           "nexus-dry-run",
		TimeoutMinutes: req.TimeoutMinutes,
	}
	// Reject early when the request is malformed locally so we do not
	// even round-trip on forms the operator can fix in the form.
	if err := probe.Validate(); err != nil {
		return DryRunResult{}, err
	}
	envIDs, err := c.resolveEnvironmentIDs(ctx, probe.Environments)
	if err != nil {
		return DryRunResult{}, err
	}
	body := createRequest{
		EnvironmentIDs: envIDs,
		InferenceModel: probe.Model,
		Name:           probe.Name,
		TeamID:         strings.TrimSpace(req.TeamID),
		EvalConfig: evalConfig{
			NumExamples:        probe.NumExamples,
			RolloutsPerExample: probe.Rollouts,
		},
	}
	if probe.TimeoutMinutes > 0 {
		body.EvalConfig.TimeoutMinutes = &probe.TimeoutMinutes
	}
	var res LaunchResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/hosted-evaluations", body, &res); err != nil {
		// The 404-on-create path is the most common useful answer
		// ("this environment is not visible to your account"), so the
		// caller can surface the message verbatim rather than parsing.
		return DryRunResult{}, err
	}
	if res.EvaluationID == "" {
		// A create that returned 2xx but no id is a vendor surprise;
		// we cannot cancel because we have nothing to cancel by.
		return DryRunResult{}, fmt.Errorf("benchmark: dry-run launch returned no evaluation id (status=%q error=%q)",
			res.Status, res.Error)
	}
	// The vendor sometimes terminalises synchronously — insufficient
	// funds is the common case — before we can cancel. An evaluation_id
	// plus a billing failure still proves slug resolution and auth.
	if dryRunAlreadySettled(res.Status) {
		if msg := strings.TrimSpace(res.Error); msg != "" && !dryRunBillingOnlyFailure(msg) {
			return DryRunResult{}, fmt.Errorf("benchmark: dry-run probe failed: %s", msg)
		}
		out := DryRunResult{}
		if msg := strings.TrimSpace(res.Error); msg != "" {
			out.Warning = msg
		}
		return out, nil
	}
	// Cancel best-effort. The vendor accepting the cancel is itself
	// part of the probe — a credential that creates but cannot cancel
	// is one we do not want to carry into a real launch — unless the
	// vendor raced us to a terminal state (409 on cancel).
	if err := c.Cancel(ctx, res.EvaluationID); err != nil {
		if dryRunCancelAlreadyTerminal(err) {
			return DryRunResult{}, nil
		}
		return DryRunResult{}, fmt.Errorf("benchmark: dry-run launch succeeded but cancel failed: %w", err)
	}
	return DryRunResult{}, nil
}

// dryRunAlreadySettled reports vendor create responses that leave
// nothing running to cancel.
func dryRunAlreadySettled(status string) bool {
	switch status {
	case primeStatusFailed, primeStatusCompleted, primeStatusCancelled, primeStatusTimeout:
		return true
	default:
		return Settled(NormalizeStatus(status))
	}
}

// dryRunBillingOnlyFailure recognises probe failures that mean
// "credential and slug worked, wallet did not".
func dryRunBillingOnlyFailure(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "insufficient funds") ||
		strings.Contains(m, "insufficient balance") ||
		strings.Contains(m, "need at least $")
}

// dryRunCancelAlreadyTerminal treats a 409 cancel refusal as probe
// success when the evaluation is already in a terminal state.
func dryRunCancelAlreadyTerminal(err error) bool {
	if err == nil {
		return true
	}
	m := strings.ToLower(err.Error())
	if !strings.Contains(m, "409") {
		return false
	}
	return strings.Contains(m, "cannot be cancelled") ||
		strings.Contains(m, "already failed") ||
		strings.Contains(m, "already completed") ||
		strings.Contains(m, "already cancelled")
}

// Status reads one run, including the aggregate score once it settles.
//
// This lives on /evaluations rather than /hosted-evaluations: the
// hosted routes only cover create, cancel and logs, and the score is
// only readable through the general evaluation record.
func (c *Client) Status(ctx context.Context, evaluationID string) (Evaluation, error) {
	if c == nil || c.token == "" {
		return Evaluation{}, ErrNoToken
	}
	if evaluationID == "" {
		return Evaluation{}, fmt.Errorf("%w: evaluation id is required", ErrInvalidRequest)
	}
	var out Evaluation
	err := c.do(ctx, http.MethodGet, "/api/v1/evaluations/"+evaluationID, nil, &out)
	return out, err
}

// Logs returns the sandbox log text, which is the only window into a
// run that is failing for a reason the status field does not explain.
func (c *Client) Logs(ctx context.Context, evaluationID string) (string, error) {
	if c == nil || c.token == "" {
		return "", ErrNoToken
	}
	var out struct {
		Logs string `json:"logs"`
	}
	err := c.do(ctx, http.MethodGet,
		"/api/v1/hosted-evaluations/"+evaluationID+"/logs", nil, &out)
	return out.Logs, err
}

// Cancel stops a running evaluation. Cancelling an already-terminal run
// is not an error worth surfacing, so vendor 4xx responses about state
// are returned as-is for the caller to interpret.
func (c *Client) Cancel(ctx context.Context, evaluationID string) error {
	if c == nil || c.token == "" {
		return ErrNoToken
	}
	var out struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := c.do(ctx, http.MethodPatch,
		"/api/v1/hosted-evaluations/"+evaluationID+"/cancel", nil, &out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("benchmark: cancel refused: %s", out.Message)
	}
	return nil
}

// Models lists the vendor's inference catalogue. Used by the console to
// offer a model picker and to show token pricing next to it.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	if c == nil || c.token == "" {
		return nil, ErrNoToken
	}
	var out struct {
		Models []Model `json:"models"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/hosted-evaluations/models", nil, &out)
	return out.Models, err
}

// do performs one request and decodes either the result or the error.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var reader io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("benchmark: encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return fmt.Errorf("benchmark: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("benchmark: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Bounded read: the logs endpoint can return a lot, and a
	// misrouted request can return a whole HTML page.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("benchmark: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return apiError(method, path, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("benchmark: decode %s %s response (HTTP %d): %w",
			method, path, resp.StatusCode, err)
	}
	return nil
}

// apiError turns a vendor failure into something an operator can act
// on. The vendor uses two error shapes — {"detail": …} for routing and
// auth failures, {"errors":[{"param","details"}]} for validation — and
// an unauthenticated proxy in front of either returns HTML. Reporting
// the raw status alone sent us chasing an ingress problem for hours on
// the Langfuse integration, so each case says what it actually was.
func apiError(method, path string, status int, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if bytes.HasPrefix(trimmed, []byte("<")) {
		return fmt.Errorf("benchmark: %s %s returned HTML with HTTP %d — the request was intercepted before reaching the API (%s)",
			method, path, status, snippet(trimmed))
	}
	var detail struct {
		Detail string `json:"detail"`
		Errors []struct {
			Param   string `json:"param"`
			Details string `json:"details"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(trimmed, &detail)

	msg := detail.Detail
	if msg == "" && len(detail.Errors) > 0 {
		parts := make([]string, 0, len(detail.Errors))
		for _, e := range detail.Errors {
			parts = append(parts, strings.TrimSpace(e.Param+": "+e.Details))
		}
		msg = strings.Join(parts, "; ")
	}
	if msg == "" {
		msg = snippet(trimmed)
	}

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("benchmark: credentials rejected (401): check the provider API key (%s)", msg)
	case http.StatusNotFound:
		// The most common cause by far is an environment slug the
		// account cannot see, so name that possibility explicitly.
		return fmt.Errorf("benchmark: not found (404): %s — for a launch this usually means the environment is not published to your account", msg)
	case http.StatusPaymentRequired:
		return fmt.Errorf("benchmark: provider refused for billing reasons (402): %s", msg)
	default:
		return fmt.Errorf("benchmark: %s %s failed with HTTP %d: %s", method, path, status, msg)
	}
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
