package evals

import "testing"

func TestScoreStoreKind(t *testing.T) {
	cases := []struct {
		ch, pg bool
		want   StoreKind
	}{
		{true, true, StoreClickHouse},
		{true, false, StoreClickHouse},
		{false, true, StorePostgres},
		{false, false, StoreNoop},
	}
	for _, c := range cases {
		if got := ScoreStoreKind(c.ch, c.pg); got != c.want {
			t.Fatalf("ch=%v pg=%v: want %q, got %q", c.ch, c.pg, c.want, got)
		}
	}
}

func TestNewScoreSink(t *testing.T) {
	sink := NewScoreSink(StoreNoop, ScoreSinkDeps{})
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("noop kind: want NoopSink, got %T", sink)
	}

	sink = NewScoreSink(StoreClickHouse, ScoreSinkDeps{})
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("clickhouse without conn: want NoopSink fallback, got %T", sink)
	}

	sink = NewScoreSink(StorePostgres, ScoreSinkDeps{})
	if _, ok := sink.(NoopSink); !ok {
		t.Fatalf("postgres without pool: want NoopSink fallback, got %T", sink)
	}
}

func TestStoreKindPersisted(t *testing.T) {
	if !StoreClickHouse.Persisted() {
		t.Fatal("clickhouse should persist")
	}
	if !StorePostgres.Persisted() {
		t.Fatal("postgres should persist")
	}
	if StoreNoop.Persisted() {
		t.Fatal("noop should not persist")
	}
}
