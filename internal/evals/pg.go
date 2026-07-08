package evals

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSink writes evaluation scores to Postgres eval_scores. Volume is low
// (sampled heuristics + judges), so each WriteScores call inserts its batch directly.
type PGSink struct {
	pool *pgxpool.Pool
}

// NewPGSink builds a Postgres-backed score sink reusing an existing pool
// (typically the control-plane store pool).
func NewPGSink(pool *pgxpool.Pool) *PGSink { return &PGSink{pool: pool} }

// WriteScores implements Sink.
func (s *PGSink) WriteScores(ctx context.Context, scores []Score) error {
	if len(scores) == 0 || s.pool == nil {
		return nil
	}
	batch := &pgx.Batch{}
	for _, sc := range scores {
		batch.Queue(
			`INSERT INTO eval_scores
			 (trace_id, timestamp, evaluator, metric, score, passed, rationale, judge_model, user_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			sc.TraceID, sc.Timestamp, sc.Evaluator, sc.Metric,
			sc.Score, sc.Passed, sc.Rationale, sc.JudgeModel, sc.UserID,
		)
	}
	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range scores {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}
