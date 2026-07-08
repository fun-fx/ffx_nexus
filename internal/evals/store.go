package evals

import (
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StoreKind identifies which ScoreStore adapter is active.
type StoreKind string

const (
	StoreClickHouse StoreKind = "clickhouse"
	StorePostgres   StoreKind = "postgres"
	StoreNoop       StoreKind = "noop"
)

// String returns the store kind as a stable API/JSON value.
func (k StoreKind) String() string { return string(k) }

// Persisted reports whether eval scores are written to durable storage.
func (k StoreKind) Persisted() bool {
	return k == StoreClickHouse || k == StorePostgres
}

// ScoreStoreKind picks the score store. ClickHouse wins when both are available
// (better analytics/routing); Postgres is the fallback for lighter deployments.
func ScoreStoreKind(chConnected, pgConnected bool) StoreKind {
	if chConnected {
		return StoreClickHouse
	}
	if pgConnected {
		return StorePostgres
	}
	return StoreNoop
}

// ScoreSinkDeps holds optional backends for NewScoreSink.
type ScoreSinkDeps struct {
	CHConn driver.Conn
	PGPool *pgxpool.Pool
}

// NewScoreSink builds a Sink for the given store kind.
func NewScoreSink(kind StoreKind, deps ScoreSinkDeps) Sink {
	switch kind {
	case StoreClickHouse:
		if deps.CHConn != nil {
			return NewCHSink(deps.CHConn)
		}
	case StorePostgres:
		if deps.PGPool != nil {
			return NewPGSink(deps.PGPool)
		}
	}
	return NoopSink{}
}
