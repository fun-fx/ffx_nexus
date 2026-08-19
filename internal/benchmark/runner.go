package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ffxnexus/nexus/internal/core"
)

// Store is the persistence the runner needs. Narrowed to the benchmark
// methods so tests can drive the lifecycle without a database and so it
// is obvious this package never touches the rest of the control plane.
type Store interface {
	CreateBenchmarkRun(ctx context.Context, r core.BenchmarkRun) error
	UpdateBenchmarkRunProgress(ctx context.Context, r core.BenchmarkRun) error
	GetBenchmarkRun(ctx context.Context, id string) (core.BenchmarkRun, error)
	ListBenchmarkRuns(ctx context.Context, orgID string, limit int) ([]core.BenchmarkRun, error)
	ListUnsettledBenchmarkRuns(ctx context.Context, limit int) ([]core.BenchmarkRun, error)
	DeleteBenchmarkRun(ctx context.Context, id string) error
	ClearBenchmarkRunVKey(ctx context.Context, id string) error
	ListRecentSettledByModel(ctx context.Context, orgID, model string, limit int) ([]core.RecentBenchmarkRun, error)
	CreateBenchmarkSchedule(ctx context.Context, r core.BenchmarkSchedule) error
	GetBenchmarkSchedule(ctx context.Context, id string) (core.BenchmarkSchedule, error)
	ListBenchmarkSchedules(ctx context.Context, orgID string, limit int) ([]core.BenchmarkSchedule, error)
	DeleteBenchmarkSchedule(ctx context.Context, id string) error
	// SetBenchmarkScheduleEnabled toggles a schedule's on/off bit
	// and re-stamps NextLaunchAt so a paused row does not sit at
	// the front of the runner's due-time scan.
	SetBenchmarkScheduleEnabled(ctx context.Context, id string, enabled bool, nextLaunchAt time.Time) error
}

// Keys mints and revokes the gateway credential handed to the provider
// for a gateway-routed run.
type Keys interface {
	CreateVirtualKey(ctx context.Context, orgID, actorID, userID, name string,
		allowedModels []string, rpm int, monthlyBudget, minQuality float64) (core.VirtualKey, string, error)
	RevokeVirtualKey(ctx context.Context, orgID, actorID, id string) error
}

// Tokens resolves the provider's own API credential and optional
// team billing context.
type Tokens interface {
	Token(ctx context.Context, provider string) (string, error)
	TeamID(ctx context.Context, provider string) (string, error)
}

// runKeyRPM bounds how fast the provider's sandbox may call us.
//
// The real spend bound is the sample cap enforced in LaunchRequest;
// this exists so a runaway harness with retries cannot turn into a
// sustained flood against the gateway.
const runKeyRPM = 120

// pollBatch limits how many runs one poll pass inspects. Each run costs
// a provider round-trip, so a large backlog is spread over several
// passes rather than stalling the loop.
const pollBatch = 50

// Runner owns the benchmark lifecycle: launch, poll to settlement, and
// cancel. It holds no state between calls — everything lives on the row
// — so several replicas can run the poller without coordination.
type Runner struct {
	store   Store
	keys    Keys
	tokens  Tokens
	log     *slog.Logger
	apiBase string
	hc      *http.Client

	// gatewayURL is this deployment's public gateway base, without the
	// /v1 suffix. Empty means gateway-routed runs are refused: the
	// provider cannot reach a URL we cannot name.
	gatewayURL string

	// Drift detection. Set via SetDriftWatcher at boot; nil keeps the
	// runner freely settling without a noise pip on every transition.
	// Alert fan-out is through the watcher's own sink so this field
	// only needs to remember the value, not deliver events itself.
	watcher       *DriftWatcher
	watcherSource DriftSources
}

// NewRunner wires a runner. A nil logger is replaced with a discarding
// one so tests and embedded uses need not supply one.
//
// Boot-time dependency contract: any nil Store / Keys / Tokens is a
// boot-time programmer error, NOT a runtime condition the runner
// should react to. Phase D-1 separates Gateway and Worker; the
// benchmark runner will, in that cutover, sit on the Worker side and
// its Store/Keys/Tokens will be Worker-side stubs that have to wire
// into the same machinery as the cron scheduler. Allowing nil into
// the runner at construction is an attractive nuisance that will
// explode the FIRST time the scheduler code path runs, which is
// far away from the wiring-debug scope; failing at boot is the
// shorter feedback loop and the same convention cmd/nexus/main.go
// already enforces for several other struct constructors (see
// validationRequiredAtBoot in docs/audit-failstop-policy.md).
//
// Gateway-attached callers (Phase D-1) MUST continue to pass a real
// Store/Keys; the panic here surfaces the wiring error before
// replicaA.go launches. Without this guard, a missing Keys argument
// in production would zero-error Launch and silently skip the
// credential cleanup phase on every shutdown.
func NewRunner(store Store, keys Keys, tokens Tokens, gatewayURL string, log *slog.Logger) *Runner {
	if store == nil {
		panic("benchmark.NewRunner: store is nil; this is a boot-time wiring error")
	}
	if keys == nil {
		panic("benchmark.NewRunner: keys is nil; this is a boot-time wiring error")
	}
	if tokens == nil {
		panic("benchmark.NewRunner: tokens is nil; this is a boot-time wiring error")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		store:      store,
		keys:       keys,
		tokens:     tokens,
		log:        log,
		gatewayURL: strings.TrimRight(strings.TrimSpace(gatewayURL), "/"),
	}
}

// SetAPIBase overrides the provider host. Only tests and a future
// self-hosted provider need this.
func (r *Runner) SetAPIBase(base string, hc *http.Client) {
	r.apiBase = base
	r.hc = hc
}

// SetDriftWatcher wires the post-settlement drift detector. Pass a
// nil watcher to disable. The source is the read interface the
// watcher consults to fetch adjacent rows; passing nil disables
// the alert path even if watcher is set.
//
// Both arguments are stored on the runner because the watcher
// itself is concurrent-safe and stateless; the runner can keep
// pointing at it from any poll goroutine.
func (r *Runner) SetDriftWatcher(w *DriftWatcher, src DriftSources) {
	r.watcher = w
	r.watcherSource = src
}

// SweepStaleness runs the boot-time staleness check across the
// listed models. The watcher emits alerts; the runner's only
// responsibility is the call (and the model's own slice).
func (r *Runner) SweepStaleness(ctx context.Context, models []string) {
	if r == nil || r.watcher == nil || r.watcherSource == nil {
		return
	}
	r.watcher.ObserveStaleness(ctx, r.watcherSource, models)
}

// GatewayRoutingAvailable reports whether this deployment can ask the
// provider to send inference back through us. The console uses it to
// explain why the option is unavailable instead of failing on submit.
func (r *Runner) GatewayRoutingAvailable() bool { return r != nil && r.gatewayURL != "" }

// LaunchSpec is the console's request to start a run.
type LaunchSpec struct {
	OrgID   string
	ActorID string
	Name    string
	// ScheduleID, when non-empty, marks the run as a scheduled-launch
	// produced by internal/cron.Runner. The runner stamps the same id
	// onto the schedule row so the two sides agree on provenance.
	ScheduleID string
	// Environments are provider Hub slugs. We cannot validate them —
	// the provider publishes no environments API — so a wrong slug
	// surfaces as their 404 with our explanation attached.
	Environments   []string
	Model          string
	NumExamples    int
	Rollouts       int
	TimeoutMinutes int
	// ViaGateway routes the provider's inference through this gateway,
	// which is what makes the score describe what we serve rather than
	// what the provider serves.
	ViaGateway bool
}

// Launch starts a run and returns the stored row.
//
// The row is written before the provider is called and updated after,
// so a launch that fails leaves a visible failed run rather than
// nothing. The order matters the other way too: the request is
// validated and the credential resolved before a virtual key is
// minted, so a rejected form never leaves a live key behind.
func (r *Runner) Launch(ctx context.Context, spec LaunchSpec) (core.BenchmarkRun, error) {
	if r == nil || r.store == nil {
		return core.BenchmarkRun{}, errors.New("benchmark: runner not configured")
	}
	token, err := r.tokens.Token(ctx, ProviderPrime)
	if err != nil {
		return core.BenchmarkRun{}, err
	}
	if token == "" {
		return core.BenchmarkRun{}, fmt.Errorf(
			"%w: paste the PrimeIntellect API key in the console before launching a run", ErrNoToken)
	}

	teamID, err := r.tokens.TeamID(ctx, ProviderPrime)
	if err != nil {
		return core.BenchmarkRun{}, err
	}
	req := LaunchRequest{
		Environments:   spec.Environments,
		Model:          spec.Model,
		NumExamples:    spec.NumExamples,
		Rollouts:       spec.Rollouts,
		Name:           spec.Name,
		TimeoutMinutes: spec.TimeoutMinutes,
		TeamID:         teamID,
		ScheduleID:     spec.ScheduleID,
	}
	// Validate before minting anything. Validate() also runs inside
	// Launch, but by then a key would already exist.
	if err := req.Validate(); err != nil {
		return core.BenchmarkRun{}, err
	}

	run := core.BenchmarkRun{
		ID:           uuid.NewString(),
		OrgID:        spec.OrgID,
		Provider:     ProviderPrime,
		Name:         spec.Name,
		Environments: spec.Environments,
		Model:        spec.Model,
		NumExamples:  spec.NumExamples,
		Rollouts:     spec.Rollouts,
		ViaGateway:   spec.ViaGateway,
		Status:       StatusPending,
		CreatedBy:    spec.ActorID,
		ScheduleID:   spec.ScheduleID,
	}

	if spec.ViaGateway {
		if r.gatewayURL == "" {
			return core.BenchmarkRun{}, fmt.Errorf(
				"%w: gateway-routed runs need NEXUS_PUBLIC_GATEWAY_URL to be set",
				ErrInvalidRequest)
		}
		vk, secret, err := r.keys.CreateVirtualKey(ctx, spec.OrgID, spec.ActorID, spec.ActorID,
			"benchmark "+shortID(run.ID),
			// Scoped to the one model under test: the provider's
			// sandbox gets no reach beyond what the run needs.
			allowedModelsForBenchmark(spec.Model), runKeyRPM, 0, 0)
		if err != nil {
			return core.BenchmarkRun{}, fmt.Errorf("benchmark: mint gateway key: %w", err)
		}
		run.VKeyID = vk.ID
		req.BaseURL = r.gatewayURL + "/v1"
		req.KeyVar = "NEXUS_API_KEY"
		req.Secrets = map[string]string{"NEXUS_API_KEY": secret}
	}

	if err := r.store.CreateBenchmarkRun(ctx, run); err != nil {
		// No provider call happened yet, so the key is the only thing
		// left dangling.
		r.revokeKey(ctx, run)
		return core.BenchmarkRun{}, fmt.Errorf("benchmark: record run: %w", err)
	}

	res, launchErr := r.client(token).Launch(ctx, req)
	if launchErr != nil {
		run.Status = StatusFailed
		run.Error = launchErr.Error()
		// Keep the external id if one came back: a partially started
		// run is still worth being able to look up.
		run.ExternalID = res.EvaluationID
		if err := r.store.UpdateBenchmarkRunProgress(ctx, run); err != nil {
			r.log.Warn("benchmark: could not record launch failure",
				"run", run.ID, "err", err)
		}
		r.revokeKey(ctx, run)
		return run, launchErr
	}

	run.ExternalID = res.EvaluationID
	run.ExternalStatus = res.Status
	run.Status = NormalizeStatus(res.Status)
	if run.Status == StatusFailed && strings.TrimSpace(res.Error) != "" {
		run.Error = strings.TrimSpace(res.Error)
	}
	if err := r.store.UpdateBenchmarkRunProgress(ctx, run); err != nil {
		// The provider is already running it. Log loudly rather than
		// fail the request: the poller will reconcile from the row, and
		// a row that lost its external id is the one case the poller
		// cannot fix.
		r.log.Error("benchmark: run started but the record could not be updated",
			"run", run.ID, "external_id", res.EvaluationID, "err", err)
	}
	r.log.Info("benchmark run launched",
		"run", run.ID, "external_id", res.EvaluationID, "model", spec.Model,
		"environments", spec.Environments, "samples", req.TotalSamples(),
		"via_gateway", spec.ViaGateway)
	return run, nil
}

// PollOnce advances every unsettled run and returns how many changed.
//
// Errors on individual runs are logged and skipped rather than aborting
// the pass: one unreachable run must not stop the others from settling.
func (r *Runner) PollOnce(ctx context.Context) (int, error) {
	if r == nil || r.store == nil {
		return 0, errors.New("benchmark: runner not configured")
	}
	runs, err := r.store.ListUnsettledBenchmarkRuns(ctx, pollBatch)
	if err != nil {
		return 0, err
	}
	if len(runs) == 0 {
		return 0, nil
	}
	token, err := r.tokens.Token(ctx, ProviderPrime)
	if err != nil || token == "" {
		// Without a credential we cannot poll. Not an error worth
		// retrying loudly every tick — the runs stay unsettled and
		// resume once a key is pasted.
		r.log.Warn("benchmark: cannot poll runs without a provider key",
			"unsettled", len(runs), "err", err)
		return 0, nil
	}
	client := r.client(token)

	changed := 0
	for _, run := range runs {
		ev, err := client.Status(ctx, run.ExternalID)
		if err != nil {
			r.log.Warn("benchmark: poll failed",
				"run", run.ID, "external_id", run.ExternalID, "err", err)
			continue
		}
		written, watch, justRun, justSettled, werr := r.applyStatus(ctx, run, ev)
		if werr != nil || !written {
			continue
		}
		changed++
		if justSettled {
			r.log.Info("benchmark run settled",
				"run", run.ID, "status", NormalizeStatus(ev.Status),
				"external_status", ev.Status,
				"avg_score", scoreForLog(ev.AvgScore), "samples", ev.TotalSamples)
		}
		if watch != nil && justRun.ID != "" {
			watch.ObserveSettle(ctx, r.watcherSource, justRun)
		}
	}
	return changed, nil
}

// applyStatus folds one provider reading into the row. Returns five
// values so the caller can decide whether to fire drift alerts and
// log "settled" independently:
//
//	written  : the row was actually updated (always true on success)
//	watch    : the drift watcher, if any (nil disables)
//	justRun  : the run-as-RunLite that just settled, set when watch is non-nil
//	settled  : true only on the exact transition from "not settled" to "settled"
//	err      : non-nil when writing the row failed
//
// Returning settled separately from written is what stops the
// post-settlement log line from firing on every poll. The settled
// transition is rare; a settled-already run still moves its
// completed_at into the picture but the log line should not.
func (r *Runner) applyStatus(ctx context.Context, run core.BenchmarkRun, ev Evaluation) (written bool, watch *DriftWatcher, justRun RunLite, settled bool, err error) {
	if r == nil {
		return false, nil, RunLite{}, false, errors.New("benchmark: runner not configured")
	}
	next := core.BenchmarkRun{
		ID:             run.ID,
		ExternalID:     run.ExternalID,
		Status:         NormalizeStatus(ev.Status),
		ExternalStatus: ev.Status,
		AvgScore:       ev.AvgScore,
		MinScore:       ev.MinScore,
		MaxScore:       ev.MaxScore,
		ViewerURL:      ev.ViewerURL,
		Error:          ev.ErrorMessage,
		StartedAt:      ev.StartedAt,
		CompletedAt:    ev.CompletedAt,
	}
	if ev.TotalSamples > 0 {
		n := ev.TotalSamples
		next.TotalSamples = &n
	}
	if len(ev.Metrics) > 0 {
		if raw, err := json.Marshal(ev.Metrics); err == nil {
			next.Metrics = raw
		} else {
			r.log.Warn("benchmark: provider metrics could not be stored",
				"run", run.ID, "err", err)
		}
	}
	if Settled(next.Status) && run.VKeyID != "" {
		run.OrgID = orgOr(run.OrgID)
		r.revokeKey(ctx, run)
		if clearErr := r.store.ClearBenchmarkRunVKey(ctx, run.ID); clearErr != nil {
			r.log.Warn("benchmark: could not clear the run's key reference",
				"run", run.ID, "err", clearErr)
		}
	}
	if writeErr := r.store.UpdateBenchmarkRunProgress(ctx, next); writeErr != nil {
		r.log.Warn("benchmark: could not store poll result", "run", run.ID, "err", writeErr)
		return false, nil, RunLite{}, false, writeErr
	}
	justSettled := Settled(next.Status) && !Settled(run.Status)
	if justSettled && ev.AvgScore != nil && ev.CompletedAt != nil && !ev.CompletedAt.IsZero() {
		justRun = RunLite{
			ID:          run.ID,
			Model:       run.Model,
			AvgScore:    *ev.AvgScore,
			CompletedAt: *ev.CompletedAt,
		}
	}
	_ = timeNow // keep symbol referenced in case future tests pin the clock
	return true, r.watcher, justRun, justSettled, nil
}

// Poll runs PollOnce on a ticker until the context is cancelled. Safe to
// run on every replica: the work is idempotent and a duplicate poll
// costs one provider read.
func (r *Runner) Poll(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.PollOnce(ctx); err != nil {
				r.log.Warn("benchmark: poll pass failed", "err", err)
			}
		}
	}
}

// DryRun verifies the credential and the environment slugs against
// the vendor without creating a billable run or persisting a row.
//
// Wrapping Client.DryRun keeps the runner in charge of the
// preconditions the console relies on (a credential resolved, a
// request shape we validate locally). We do not let the caller
// probe the vendor without first walking the same checks
// Launch walks; otherwise the probe can succeed on inputs that
// would be rejected at launch time.
func (r *Runner) DryRun(ctx context.Context, spec LaunchSpec) (DryRunResult, error) {
	if r == nil || r.store == nil {
		return DryRunResult{}, errors.New("benchmark: runner not configured")
	}
	token, err := r.tokens.Token(ctx, ProviderPrime)
	if err != nil {
		return DryRunResult{}, err
	}
	if token == "" {
		return DryRunResult{}, fmt.Errorf(
			"%w: paste the PrimeIntellect API key in the console before validating environments", ErrNoToken)
	}
	teamID, err := r.tokens.TeamID(ctx, ProviderPrime)
	if err != nil {
		return DryRunResult{}, err
	}
	req := LaunchRequest{
		Environments: spec.Environments,
		Model:        spec.Model,
		TeamID:       teamID,
	}
	// Validate the *probe* shape: 1 example, 1 rollout, no
	// gateway. The runner uses the operator's spec purely to
	// forward the slug list and model name; the rest is
	// overridden by Client.DryRun.
	probe := LaunchRequest{
		Environments: req.Environments,
		Model:        req.Model,
		NumExamples:  1,
		Rollouts:     1,
		Name:         "nexus-dry-run",
	}
	if err := probe.Validate(); err != nil {
		return DryRunResult{}, err
	}
	return r.client(token).DryRun(ctx, req)
}

// Cancel stops a running evaluation at the provider and settles the row.
func (r *Runner) Cancel(ctx context.Context, id string) error {
	if r == nil || r.store == nil {
		return errors.New("benchmark: runner not configured")
	}
	run, err := r.store.GetBenchmarkRun(ctx, id)
	if err != nil {
		return err
	}
	if Settled(run.Status) {
		return fmt.Errorf("%w: run is already %s", ErrConflict, run.Status)
	}
	if run.ExternalID != "" {
		token, err := r.tokens.Token(ctx, ProviderPrime)
		if err != nil {
			return err
		}
		if token == "" {
			return ErrNoToken
		}
		if err := r.client(token).Cancel(ctx, run.ExternalID); err != nil {
			// Fall through to settling our row anyway: the operator
			// asked to stop, and leaving the row "running" forever
			// after a provider-side race is worse than a stale record.
			r.log.Warn("benchmark: provider refused cancel; settling the local record anyway",
				"run", id, "err", err)
		}
	}
	r.revokeKey(ctx, run)
	next := core.BenchmarkRun{ID: run.ID, Status: StatusCancelled, ExternalStatus: run.ExternalStatus}
	if err := r.store.UpdateBenchmarkRunProgress(ctx, next); err != nil {
		return err
	}
	if err := r.store.ClearBenchmarkRunVKey(ctx, run.ID); err != nil {
		r.log.Warn("benchmark: could not clear the run's key reference", "run", run.ID, "err", err)
	}
	return nil
}

// List and Get pass through to the store so the console depends on one
// interface rather than two.
func (r *Runner) List(ctx context.Context, orgID string, limit int) ([]core.BenchmarkRun, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("benchmark: runner not configured")
	}
	return r.store.ListBenchmarkRuns(ctx, orgID, limit)
}

func (r *Runner) Get(ctx context.Context, id string) (core.BenchmarkRun, error) {
	if r == nil || r.store == nil {
		return core.BenchmarkRun{}, errors.New("benchmark: runner not configured")
	}
	return r.store.GetBenchmarkRun(ctx, id)
}

// Delete forgets our record. A run still live at the provider is
// cancelled first so deleting from the console cannot leave a billable
// job running with nothing tracking it.
func (r *Runner) Delete(ctx context.Context, id string) error {
	if r == nil || r.store == nil {
		return errors.New("benchmark: runner not configured")
	}
	run, err := r.store.GetBenchmarkRun(ctx, id)
	if err != nil {
		return err
	}
	if !Settled(run.Status) {
		if err := r.Cancel(ctx, id); err != nil {
			r.log.Warn("benchmark: could not cancel before delete", "run", id, "err", err)
		}
	}
	r.revokeKey(ctx, run)
	return r.store.DeleteBenchmarkRun(ctx, id)
}

// Logs returns the provider's sandbox log for a run.
func (r *Runner) Logs(ctx context.Context, id string) (string, error) {
	if r == nil || r.store == nil {
		return "", errors.New("benchmark: runner not configured")
	}
	run, err := r.store.GetBenchmarkRun(ctx, id)
	if err != nil {
		return "", err
	}
	if run.ExternalID == "" {
		return "", fmt.Errorf(
			"%w: this run never started at the provider, so it has no logs", ErrInvalidRequest)
	}
	token, err := r.tokens.Token(ctx, ProviderPrime)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", ErrNoToken
	}
	return r.client(token).Logs(ctx, run.ExternalID)
}

// Models lists the provider's inference catalogue for the console
// picker.
func (r *Runner) Models(ctx context.Context) ([]Model, error) {
	token, err := r.tokens.Token(ctx, ProviderPrime)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrNoToken
	}
	return r.client(token).Models(ctx)
}

func (r *Runner) client(token string) *Client {
	return NewClient(r.apiBase, token, r.hc)
}

// CreateSchedule wraps the store with a runner-side nil-guard so a
// misconfigured Server returns 503 cleanly. The store fills in the
// default NextLaunchAt when the caller has not supplied one — the
// console always supplies, but a programmatic caller might leave
// it zero and the subsequent cron tick would then fire immediately.
func (r *Runner) CreateSchedule(ctx context.Context, row core.BenchmarkSchedule) (core.BenchmarkSchedule, error) {
	if r == nil || r.store == nil {
		return core.BenchmarkSchedule{}, errors.New("benchmark: runner not configured")
	}
	if row.NextLaunchAt.IsZero() && row.CadenceSeconds > 0 {
		row.NextLaunchAt = timeNow().Add(time.Duration(row.CadenceSeconds) * time.Second)
	}
	if err := r.store.CreateBenchmarkSchedule(ctx, row); err != nil {
		return core.BenchmarkSchedule{}, err
	}
	return r.store.GetBenchmarkSchedule(ctx, row.ID)
}

// ListSchedules mirrors List so the console can fetch the
// operator's plans with the same nil-guard.
func (r *Runner) ListSchedules(ctx context.Context, orgID string, limit int) ([]core.BenchmarkSchedule, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("benchmark: runner not configured")
	}
	return r.store.ListBenchmarkSchedules(ctx, orgID, limit)
}

func (r *Runner) GetSchedule(ctx context.Context, id string) (core.BenchmarkSchedule, error) {
	if r == nil || r.store == nil {
		return core.BenchmarkSchedule{}, errors.New("benchmark: runner not configured")
	}
	return r.store.GetBenchmarkSchedule(ctx, id)
}

func (r *Runner) DeleteSchedule(ctx context.Context, id string) error {
	if r == nil || r.store == nil {
		return errors.New("benchmark: runner not configured")
	}
	return r.store.DeleteBenchmarkSchedule(ctx, id)
}

// SetScheduleEnabled toggles a schedule on or off and re-stamps
// NextLaunchAt so a paused row does not sit at the front of the
// runner's scan queue. Resume fires a fresh launch one cadence out,
// not the one that was already due when the row was paused: a paused
// run with a stale timestamp would otherwise drown the queue.
func (r *Runner) SetScheduleEnabled(
	ctx context.Context, id string, enabled bool, nextLaunchAt time.Time,
) error {
	if r == nil || r.store == nil {
		return errors.New("benchmark: runner not configured")
	}
	if id == "" {
		return fmt.Errorf("%w: schedule id is required", ErrInvalidRequest)
	}
	return r.store.SetBenchmarkScheduleEnabled(ctx, id, enabled, nextLaunchAt)
}

// GetLatestSettledByModel is the leaderboard's primary input.
// Returns ErrNotFound when the model has never settled a row, just
// like the underlying store.
//
// orgID scopes the lookup to one tenant; see Store.ListRecentSettledByModel for
// why a benchmark score is tenant data and not shared operational health.
func (r *Runner) GetLatestSettledByModel(ctx context.Context, orgID, model string) (core.BenchmarkRun, error) {
	if r == nil || r.store == nil {
		return core.BenchmarkRun{}, errors.New("benchmark: runner not configured")
	}
	if model == "" {
		return core.BenchmarkRun{}, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	rows, err := r.store.ListRecentSettledByModel(ctx, orgID, model, 1)
	if err != nil {
		return core.BenchmarkRun{}, err
	}
	if len(rows) == 0 {
		return core.BenchmarkRun{}, core.ErrNotFound
	}
	head, err := r.store.GetBenchmarkRun(ctx, rows[0].ID)
	if err != nil {
		return core.BenchmarkRun{}, err
	}
	return head, nil
}

// ListRecentSettledByModel serves the history endpoint. The runner
// pass-through is wrapped with the same nil-guard the rest of the
// BenchmarkRunner interface uses.
func (r *Runner) ListRecentSettledByModel(ctx context.Context, orgID, model string, limit int) ([]core.RecentBenchmarkRun, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("benchmark: runner not configured")
	}
	return r.store.ListRecentSettledByModel(ctx, orgID, model, limit)
}

// timeNow is extracted so the tests can swap a deterministic clock in
// if they need to assert on NextLaunchAt arithmetic.
var timeNow = func() time.Time { return time.Now().UTC() }

// revokeKey drops the gateway credential a run was given. Best effort:
// a failure is logged, not returned, because it must never turn a
// successful launch or settle into an error. The key is scoped to one
// model and rate limited, so a leaked one is bounded.
func (r *Runner) revokeKey(ctx context.Context, run core.BenchmarkRun) {
	if run.VKeyID == "" || r.keys == nil {
		return
	}
	if err := r.keys.RevokeVirtualKey(ctx, orgOr(run.OrgID), run.CreatedBy, run.VKeyID); err != nil {
		r.log.Warn("benchmark: could not revoke the run's gateway key",
			"run", run.ID, "vkey", run.VKeyID, "err", err)
	}
}

// orgOr mirrors CreateVirtualKey's own defaulting so revoke audits land
// in the same org the key was created under.
func orgOr(orgID string) string {
	if orgID == "" {
		return "default"
	}
	return orgID
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// allowedModelsForBenchmark lists the model ids a gateway-routed run may
// call. The hub slug and its bare upstream id are both included because
// Prime's verifiers often omit the provider/ prefix on custom api_base_url
// calls.
func allowedModelsForBenchmark(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	out := []string{model}
	if _, rest, ok := strings.Cut(model, "/"); ok && rest != "" {
		out = append(out, rest)
	}
	return out
}

func scoreForLog(v *float64) any {
	if v == nil {
		return "none"
	}
	return *v
}
