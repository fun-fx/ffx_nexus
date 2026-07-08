package evals

import "testing"

func TestScoreStoreKind(t *testing.T) {
	if got := ScoreStoreKind(true); got != StoreClickHouse {
		t.Fatalf("connected: want %q, got %q", StoreClickHouse, got)
	}
	if got := ScoreStoreKind(false); got != StoreNoop {
		t.Fatalf("disconnected: want %q, got %q", StoreNoop, got)
	}
}

func TestNewScoreSink(t *testing.T) {
	sink := NewScoreSink(StoreNoop, nil)
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("noop kind: want NoopSink, got %T", sink)
	}

	sink = NewScoreSink(StoreClickHouse, nil)
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("clickhouse without conn: want NoopSink fallback, got %T", sink)
	}
}

func TestStoreKindPersisted(t *testing.T) {
	if !StoreClickHouse.Persisted() {
		t.Fatal("clickhouse should persist")
	}
	if StoreNoop.Persisted() {
		t.Fatal("noop should not persist")
	}
}
