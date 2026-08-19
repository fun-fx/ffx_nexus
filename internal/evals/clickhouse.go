package evals

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHSink writes evaluation scores to the ClickHouse eval_scores table. Eval
// volume is low (sampled), so each WriteScores call inserts its batch directly.
type CHSink struct {
	conn driver.Conn
}

// NewCHSink builds a ClickHouse-backed score sink reusing an existing
// connection (e.g. the trace recorder's pool).
func NewCHSink(conn driver.Conn) *CHSink { return &CHSink{conn: conn} }

// WriteScores implements Sink.
func (s *CHSink) WriteScores(ctx context.Context, scores []Score) error {
	if len(scores) == 0 {
		return nil
	}
	// Columns are named explicitly. A bare `INSERT INTO eval_scores` binds
	// positionally against whatever column order the table happens to have, so
	// adding org_id at the end of the table silently shifts every value if the
	// binary and the schema disagree about the count — and a score row landing in
	// the wrong org is exactly the failure this column exists to prevent.
	batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO eval_scores (
		trace_id, timestamp, evaluator, metric, score, passed, rationale,
		judge_model, user_id, org_id)`)
	if err != nil {
		return err
	}
	for _, sc := range scores {
		if err := batch.Append(
			sc.TraceID, sc.Timestamp, sc.Evaluator, sc.Metric,
			sc.Score, boolToUint8(sc.Passed), sc.Rationale, sc.JudgeModel,
			sc.UserID, orgOrDefault(sc.OrgID),
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
