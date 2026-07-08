package evals

import (
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// StoreKind identifies which ScoreStore adapter is active.
type StoreKind string

const (
	StoreClickHouse StoreKind = "clickhouse"
	StoreNoop       StoreKind = "noop"
)

// String returns the store kind as a stable API/JSON value.
func (k StoreKind) String() string { return string(k) }

// Persisted reports whether eval scores are written to durable storage.
func (k StoreKind) Persisted() bool { return k == StoreClickHouse }

// ScoreStoreKind picks the score store from ClickHouse trace persistence.
func ScoreStoreKind(chConnected bool) StoreKind {
	if chConnected {
		return StoreClickHouse
	}
	return StoreNoop
}

// NewScoreSink builds a Sink for the given store kind. When kind is
// clickhouse but chConn is nil, it falls back to NoopSink.
func NewScoreSink(kind StoreKind, chConn driver.Conn) Sink {
	switch kind {
	case StoreClickHouse:
		if chConn != nil {
			return NewCHSink(chConn)
		}
	}
	return NoopSink{}
}
