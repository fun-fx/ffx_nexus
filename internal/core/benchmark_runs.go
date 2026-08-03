package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Benchmark runs: the durable record of a model-level evaluation we
// asked an external platform to execute.
//
// These are not eval_scores rows. An eval score is attached to a trace
// that flowed through the gateway; a benchmark result describes a model
// against a dataset and has no trace to attach to. Keeping them apart
// means the quality-routing reader cannot accidentally average a
// benchmark aggregate into per-request scores — connecting the two is a
// deliberate decision, not a side effect of storage layout.

// BenchmarkRun is one launched run and whatever the provider has told
// us about it so far.
type BenchmarkRun struct {
	ID       string `json:"id"`
	OrgID    string `json:"org_id"`
	Provider string `json:"provider"`
	// ExternalID is the provider's evaluation id. Empty while a launch
	// is in flight, and stays empty for a launch that was refused.
	ExternalID   string   `json:"external_id"`
	Name         string   `json:"name"`
	Environments []string `json:"environments"`
	Model        string   `json:"model"`
	NumExamples  int      `json:"num_examples"`
	Rollouts     int      `json:"rollouts"`
	// ViaGateway records that the provider was told to send inference
	// back through Nexus, which changes what the score means.
	ViaGateway bool `json:"via_gateway"`
	// VKeyID is the virtual key minted for that access, kept so the
	// run can revoke it when the provider is done with it.
	VKeyID         string          `json:"vkey_id,omitempty"`
	Status         string          `json:"status"`
	ExternalStatus string          `json:"external_status,omitempty"`
	AvgScore       *float64        `json:"avg_score"`
	MinScore       *float64        `json:"min_score"`
	MaxScore       *float64        `json:"max_score"`
	TotalSamples   *int            `json:"total_samples"`
	Metrics        json.RawMessage `json:"metrics,omitempty"`
	ViewerURL      string          `json:"viewer_url,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedBy      string          `json:"created_by,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

const benchmarkRunColumns = `
	id, org_id, provider, external_id, name, environments, model,
	num_examples, rollouts, via_gateway, vkey_id, status, external_status,
	avg_score, min_score, max_score, total_samples, metrics, viewer_url,
	error, created_by, created_at, updated_at, started_at, completed_at`

// CreateBenchmarkRun inserts the row before the provider is called, so
// a launch that fails mid-flight still leaves a record an operator can
// see and retry rather than vanishing.
func (s *Store) CreateBenchmarkRun(ctx context.Context, r BenchmarkRun) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	if r.ID == "" {
		return errors.New("core: benchmark run id is required")
	}
	if r.Model == "" {
		return errors.New("core: benchmark run model is required")
	}
	if r.Provider == "" {
		return errors.New("core: benchmark run provider is required")
	}
	if r.Status == "" {
		return errors.New("core: benchmark run status is required")
	}
	// A nil slice would violate the NOT NULL default on a text[].
	envs := r.Environments
	if envs == nil {
		envs = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO benchmark_runs (
			id, org_id, provider, external_id, name, environments, model,
			num_examples, rollouts, via_gateway, vkey_id, status, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		r.ID, r.OrgID, r.Provider, r.ExternalID, r.Name, envs, r.Model,
		r.NumExamples, r.Rollouts, r.ViaGateway, r.VKeyID, r.Status, r.CreatedBy)
	return err
}

// UpdateBenchmarkRunProgress writes everything the provider can change
// about a run: its identifier once the launch lands, its status on each
// poll, and the aggregate when it settles.
//
// One method covers both the post-launch and post-poll writes because
// splitting them tempted every caller into a read-modify-write, and two
// pollers racing on the same row would then lose one of the updates.
func (s *Store) UpdateBenchmarkRunProgress(ctx context.Context, r BenchmarkRun) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	if r.ID == "" {
		return errors.New("core: benchmark run id is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE benchmark_runs SET
			external_id      = COALESCE(NULLIF($2, ''), external_id),
			status           = $3,
			external_status  = $4,
			avg_score        = $5,
			min_score        = $6,
			max_score        = $7,
			total_samples    = $8,
			metrics          = COALESCE($9, metrics),
			viewer_url       = COALESCE(NULLIF($10, ''), viewer_url),
			error            = $11,
			started_at       = COALESCE($12, started_at),
			completed_at     = COALESCE($13, completed_at),
			updated_at       = NOW()
		WHERE id = $1`,
		r.ID, r.ExternalID, r.Status, r.ExternalStatus,
		r.AvgScore, r.MinScore, r.MaxScore, r.TotalSamples,
		nullableJSON(r.Metrics), r.ViewerURL, r.Error, r.StartedAt, r.CompletedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearBenchmarkRunVKey forgets the minted key reference once it has
// been revoked, so a later cleanup pass does not try again.
func (s *Store) ClearBenchmarkRunVKey(ctx context.Context, id string) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE benchmark_runs SET vkey_id = '', updated_at = NOW() WHERE id = $1`, id)
	return err
}

// GetBenchmarkRun reads one run by id.
func (s *Store) GetBenchmarkRun(ctx context.Context, id string) (BenchmarkRun, error) {
	if s == nil || s.pool == nil {
		return BenchmarkRun{}, errors.New("core: store not configured")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+benchmarkRunColumns+` FROM benchmark_runs WHERE id = $1`, id)
	if err != nil {
		return BenchmarkRun{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return BenchmarkRun{}, err
		}
		return BenchmarkRun{}, ErrNotFound
	}
	return scanBenchmarkRun(rows)
}

// ListBenchmarkRuns returns an org's runs, newest first. An empty orgID
// lists every org's runs, which is what the cluster-wide console view
// and the poller both need.
func (s *Store) ListBenchmarkRuns(ctx context.Context, orgID string, limit int) ([]BenchmarkRun, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("core: store not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows pgx.Rows
		err  error
	)
	if orgID == "" {
		rows, err = s.pool.Query(ctx, `SELECT `+benchmarkRunColumns+`
			FROM benchmark_runs ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.pool.Query(ctx, `SELECT `+benchmarkRunColumns+`
			FROM benchmark_runs WHERE org_id = $1
			ORDER BY created_at DESC LIMIT $2`, orgID, limit)
	}
	if err != nil {
		return nil, err
	}
	return collectBenchmarkRuns(rows)
}

// ListUnsettledBenchmarkRuns returns runs still worth polling, across
// every org. Rows with no external id are skipped: a launch that never
// returned an identifier has nothing to poll for.
func (s *Store) ListUnsettledBenchmarkRuns(ctx context.Context, limit int) ([]BenchmarkRun, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("core: store not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+benchmarkRunColumns+`
		FROM benchmark_runs
		WHERE status IN ('pending', 'running') AND external_id <> ''
		ORDER BY updated_at ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return collectBenchmarkRuns(rows)
}

// DeleteBenchmarkRun removes a run's record. The provider keeps its own
// copy; this only forgets ours.
func (s *Store) DeleteBenchmarkRun(ctx context.Context, id string) error {
	if s == nil || s.pool == nil {
		return errors.New("core: store not configured")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM benchmark_runs WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func collectBenchmarkRuns(rows pgx.Rows) ([]BenchmarkRun, error) {
	defer rows.Close()
	out := []BenchmarkRun{}
	for rows.Next() {
		r, err := scanBenchmarkRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanBenchmarkRun(rows pgx.Rows) (BenchmarkRun, error) {
	var r BenchmarkRun
	var metrics []byte
	if err := rows.Scan(
		&r.ID, &r.OrgID, &r.Provider, &r.ExternalID, &r.Name, &r.Environments, &r.Model,
		&r.NumExamples, &r.Rollouts, &r.ViaGateway, &r.VKeyID, &r.Status, &r.ExternalStatus,
		&r.AvgScore, &r.MinScore, &r.MaxScore, &r.TotalSamples, &metrics, &r.ViewerURL,
		&r.Error, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.StartedAt, &r.CompletedAt,
	); err != nil {
		return BenchmarkRun{}, err
	}
	if len(metrics) > 0 {
		r.Metrics = metrics
	}
	return r, nil
}

// nullableJSON keeps an absent metrics blob out of the UPDATE so the
// COALESCE preserves whatever was already stored. Passing an empty
// []byte would fail the jsonb cast instead.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}
