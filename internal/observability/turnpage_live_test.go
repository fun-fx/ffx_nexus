package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests execute the turn roll-up against a real ClickHouse. That
// matters more here than anywhere else in this package: the first cut of
// buildTurnPageQuery aliased its aggregates after the columns they summed
// (`sum(input_tokens) AS input_tokens`), ClickHouse resolved a later
// reference to the alias instead of the column, and every request failed
// with "Aggregate function sum(input_tokens) is found inside another
// aggregate function". The SQL-shape unit tests passed the whole time,
// because a string can't tell you what the server thinks the string means.
//
// Skips when no ClickHouse is reachable, so `go test ./...` still works on
// a laptop with nothing running. CI's Integration workflow supplies one.
func chTestDSN() (dsn string, explicit bool) {
	if d := os.Getenv("NEXUS_CLICKHOUSE_URL"); d != "" {
		return d, true
	}
	return "clickhouse://nexus:nexus@localhost:9000/nexus", false
}

// newLiveReader connects, applies the gateway_traces migrations, and returns
// a Reader plus the recorder used to seed rows.
func newLiveReader(t *testing.T) (*Reader, *CHRecorder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dsn, explicit := chTestDSN()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec, err := NewCHRecorder(ctx, dsn, CHOptions{}, log)
	if err != nil {
		// Skipping is right on a laptop with nothing running, but in CI the
		// DSN is set on purpose and a silent skip would turn this whole
		// safety net into a green no-op — exactly the failure mode that let
		// the aliasing bug reach prod. Fail loudly when asked explicitly.
		if explicit {
			t.Fatalf("NEXUS_CLICKHOUSE_URL is set but clickhouse is unreachable at %s: %v", dsn, err)
		}
		t.Skipf("clickhouse not reachable at %s (set NEXUS_CLICKHOUSE_URL to require it): %v", dsn, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = rec.Close(cctx)
	})

	// Walk the migration directory rather than listing files inline so a
	// new gateway_traces migration is exercised here the day it lands.
	// Other tables (benchmark_runs) are out of scope and their DDL does not
	// survive Migrate's naive semicolon split.
	root := filepath.Join("..", "..", "migrations", "clickhouse")
	files, err := filepath.Glob(filepath.Join(root, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no clickhouse migrations found under %s: %v", root, err)
	}
	for _, f := range files {
		script, readErr := os.ReadFile(f)
		if readErr != nil {
			t.Fatalf("read %s: %v", f, readErr)
		}
		if !strings.Contains(string(script), "gateway_traces") {
			continue
		}
		if err := rec.Migrate(ctx, string(script)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(f), err)
		}
	}
	return rec.NewReader(), rec
}

// seedTurn writes calls sharing one turn id. It goes through the recorder's
// synchronous insert rather than Record + Close: Close tears down the shared
// connection the Reader is about to query on, and can only be called once.
func seedTurn(t *testing.T, rec *CHRecorder, orgID, turnID, userID string, calls []Trace) {
	t.Helper()
	for i := range calls {
		calls[i].TurnID = turnID
		calls[i].UserID = userID
		calls[i].OrgID = orgID
		if calls[i].TraceID == "" {
			calls[i].TraceID = fmt.Sprintf("%s-call-%d", turnID, i)
		}
		if calls[i].Timestamp.IsZero() {
			calls[i].Timestamp = time.Now().Add(time.Duration(i) * time.Second)
		}
	}
	if err := rec.insert(calls); err != nil {
		t.Fatalf("seed insert: %v", err)
	}
}

// The regression test for the bug above: the query has to survive contact
// with a real server, and the aggregates have to add up.
func TestTurnPage_LiveGroupsCallsIntoOneRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live clickhouse test in short mode")
	}
	reader, rec := newLiveReader(t)

	turnID := fmt.Sprintf("live-turn-%d", time.Now().UnixNano())
	userID := turnID + "-user"
	org := turnID + "-org"
	start := time.Now().Add(-2 * time.Minute)

	seedTurn(t, rec, org, turnID, userID, []Trace{
		{
			Timestamp: start, ProviderName: "grid", RequestModel: "code-prime",
			InputTokens: 1000, OutputTokens: 100, LatencyMs: 1500,
			CostUSD: 0.01, StatusCode: 200,
		},
		{
			Timestamp: start.Add(5 * time.Second), ProviderName: "grid", RequestModel: "code-prime",
			InputTokens: 2000, OutputTokens: 200, LatencyMs: 2500,
			CostUSD: 0.02, StatusCode: 200,
		},
		{
			Timestamp: start.Add(10 * time.Second), ProviderName: "grid", RequestModel: "code-prime",
			InputTokens: 3000, OutputTokens: 300, LatencyMs: 3000,
			CostUSD: 0.03, StatusCode: 500,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turns, err := reader.TurnPage(ctx, time.Time{}, start.Add(-time.Minute), 50, org, userID)
	if err != nil {
		t.Fatalf("TurnPage against live clickhouse: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("three calls of one turn must collapse to one row, got %d: %+v", len(turns), turns)
	}

	got := turns[0]
	if got.TurnID != turnID {
		t.Errorf("turn id: got %q want %q", got.TurnID, turnID)
	}
	if got.TraceCount != 3 {
		t.Errorf("trace count: got %d want 3", got.TraceCount)
	}
	if got.InputTokens != 6000 || got.OutputTokens != 600 {
		t.Errorf("token sums: got in=%d out=%d want in=6000 out=600", got.InputTokens, got.OutputTokens)
	}
	if got.TotalTokens != 6600 {
		t.Errorf("total tokens: got %d want 6600", got.TotalTokens)
	}
	if got.LatencyMs != 7000 {
		t.Errorf("latency must be the summed wall time: got %d want 7000", got.LatencyMs)
	}
	// The whole point of max(): one failure inside a green loop stays visible
	// on the collapsed row.
	if got.StatusCode != 500 {
		t.Errorf("status must be the worst in the turn: got %d want 500", got.StatusCode)
	}
	if got.FirstAt.After(got.LastAt) {
		t.Errorf("first_at %v must not be after last_at %v", got.FirstAt, got.LastAt)
	}
}

// Two turns from the same user must stay apart, and the newest has to sort
// first — the overview reads top-down.
func TestTurnPage_LiveKeepsSeparateTurnsApart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live clickhouse test in short mode")
	}
	reader, rec := newLiveReader(t)

	stamp := time.Now().UnixNano()
	userID := fmt.Sprintf("live-user-%d", stamp)
	org := fmt.Sprintf("live-org-%d", stamp)
	older := fmt.Sprintf("live-turn-a-%d", stamp)
	newer := fmt.Sprintf("live-turn-b-%d", stamp)
	base := time.Now().Add(-2 * time.Minute)

	seedTurn(t, rec, org, older, userID, []Trace{
		{Timestamp: base, ProviderName: "grid", RequestModel: "code-prime", StatusCode: 200},
	})
	seedTurn(t, rec, org, newer, userID, []Trace{
		{Timestamp: base.Add(30 * time.Second), ProviderName: "grid", RequestModel: "text-prime", StatusCode: 200},
		{Timestamp: base.Add(35 * time.Second), ProviderName: "grid", RequestModel: "text-prime", StatusCode: 200},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turns, err := reader.TurnPage(ctx, time.Time{}, base.Add(-time.Minute), 50, org, userID)
	if err != nil {
		t.Fatalf("TurnPage: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("two distinct turns must not merge, got %d: %+v", len(turns), turns)
	}
	if turns[0].TurnID != newer {
		t.Errorf("most recent turn must sort first: got %q want %q", turns[0].TurnID, newer)
	}
	if turns[0].TraceCount != 2 || turns[1].TraceCount != 1 {
		t.Errorf("call counts: got %d and %d want 2 and 1", turns[0].TraceCount, turns[1].TraceCount)
	}
}

// Rows written before 008_turn_id.sql carry an empty turn_id. They must fall
// back to their own trace_id so historical traffic renders one-per-row
// instead of collapsing into a single meaningless group.
func TestTurnPage_LiveUngroupedRowsFallBackToTraceID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live clickhouse test in short mode")
	}
	reader, rec := newLiveReader(t)

	stamp := time.Now().UnixNano()
	userID := fmt.Sprintf("live-legacy-%d", stamp)
	org := fmt.Sprintf("live-legacy-org-%d", stamp)
	base := time.Now().Add(-2 * time.Minute)

	seedTurn(t, rec, org, "", userID, []Trace{
		{TraceID: fmt.Sprintf("legacy-a-%d", stamp), Timestamp: base, ProviderName: "grid", RequestModel: "code-prime", StatusCode: 200},
		{TraceID: fmt.Sprintf("legacy-b-%d", stamp), Timestamp: base.Add(time.Second), ProviderName: "grid", RequestModel: "code-prime", StatusCode: 200},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turns, err := reader.TurnPage(ctx, time.Time{}, base.Add(-time.Minute), 50, org, userID)
	if err != nil {
		t.Fatalf("TurnPage: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("empty turn_id must not collapse rows together, got %d: %+v", len(turns), turns)
	}
	for _, got := range turns {
		if got.TraceCount != 1 {
			t.Errorf("legacy row %q must stand alone, got count %d", got.TurnID, got.TraceCount)
		}
	}
}

// The drill-down path: /api/traces?turn=<id> has to return exactly the
// calls of that turn and nothing else.
func TestTracePage_LiveTurnFilterReturnsOnlyThatTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live clickhouse test in short mode")
	}
	reader, rec := newLiveReader(t)

	stamp := time.Now().UnixNano()
	userID := fmt.Sprintf("live-drill-%d", stamp)
	org := fmt.Sprintf("live-drill-org-%d", stamp)
	wanted := fmt.Sprintf("live-drill-turn-%d", stamp)
	other := fmt.Sprintf("live-other-turn-%d", stamp)
	base := time.Now().Add(-2 * time.Minute)

	seedTurn(t, rec, org, wanted, userID, []Trace{
		{Timestamp: base, ProviderName: "grid", RequestModel: "code-prime", StatusCode: 200},
		{Timestamp: base.Add(time.Second), ProviderName: "grid", RequestModel: "code-prime", StatusCode: 200},
	})
	seedTurn(t, rec, org, other, userID, []Trace{
		{Timestamp: base.Add(2 * time.Second), ProviderName: "grid", RequestModel: "text-prime", StatusCode: 200},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := reader.TracePage(ctx, time.Time{}, base.Add(-time.Minute), 50, org, userID, TraceFilter{Turn: wanted})
	if err != nil {
		t.Fatalf("TracePage with turn filter: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("drill-down must return the turn's 2 calls, got %d", len(page.Items))
	}
	for _, it := range page.Items {
		if it.TurnID != wanted {
			t.Errorf("leaked a call from another turn: %q", it.TurnID)
		}
	}
}
