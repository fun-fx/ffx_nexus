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
	batch, err := s.conn.PrepareBatch(ctx, `INSERT INTO eval_scores`)
	if err != nil {
		return err
	}
	for _, sc := range scores {
		if err := batch.Append(
			sc.TraceID, sc.Timestamp, sc.Evaluator, sc.Metric,
			sc.Score, boolToUint8(sc.Passed), sc.Rationale, sc.JudgeModel,
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
