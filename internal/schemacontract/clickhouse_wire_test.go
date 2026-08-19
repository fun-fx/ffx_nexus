// Layer B — ClickHouse wire smoke (build tag clickhouse_smoke).
// The companion Layer A in clickhouse_smoke_test.go runs in
// default CI on every PR. This file boots a real ClickHouse
// connection (controlled by NEXUS_TEST_CLICKHOUSE_URL) and runs
// the schema-contract that the in-process guard cannot: DESCRIBE
// for every ClickHouse table we touch, plus an INSERT round-trip
// that proves the column-set in code matches what the migrations
// declare live.
//
// Nightly CI: set NEXUS_TEST_CLICKHOUSE_URL and run
//
//	go test -tags=clickhouse_smoke ./internal/schemacontract -v
//
// The integration test refuses to run as part of the unit test
// suite without the tag, so a developer laptop doesn't need a
// ClickHouse container to keep CI green.

//go:build clickhouse_smoke
// +build clickhouse_smoke

package schemacontract

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// The list of ClickHouse tables the repository writes to or reads
// from. This list is the seed truth the wire smoke validates
// against; mutations to it must be reflected in
// docs/clickhouse-verification.md.
var clickhouseTables = []string{
	"gateway_traces",
	"eval_scores",
}

// requiredColumnsForGatewayTraces lists the minimum contract
// for the most-touched table. A column the Go code names must
// appear here; the list is the worst-case signal a missing column
// produces for downstream customers.
var requiredColumnsForGatewayTraces = []string{
	"trace_id", "span_id", "parent_span_id", "timestamp",
	"org_id", "virtual_key_id",
	"operation_name", "provider_name", "request_model", "response_model",
	"input_tokens", "output_tokens", "finish_reason",
	"temperature", "top_p", "max_tokens",
	"streamed", "ttft_ms", "latency_ms", "cost_usd",
	"status_code",
}

func TestClickHouseTablesExist(t *testing.T) {
	url := os.Getenv("NEXUS_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_CLICKHOUSE_URL to run the wire smoke")
	}
	ctx := context.Background()
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{url}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	for _, table := range clickhouseTables {
		rows, err := conn.Query(ctx,
			`SELECT name FROM system.columns WHERE database = currentDatabase() AND table = ?`,
			table)
		if err != nil {
			t.Fatalf("system.columns for %s: %v", table, err)
		}
		var cols []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			cols = append(cols, c)
		}
		rows.Close()
		if len(cols) == 0 {
			t.Fatalf("table %s has no columns — migrations did not run "+
				"or the table name here does not match the migration set",
				table)
		}
		if table == "gateway_traces" {
			for _, must := range requiredColumnsForGatewayTraces {
				if !contains(cols, must) {
					t.Errorf("gateway_traces column %q is referenced by code but the "+
						"live schema does not declare it; either the migration set "+
						"drifted or the code uses a stale name", must)
				}
			}
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestClickHouseWriteSmoke exercises the INSERT shape code uses.
// If the column list in code has drifted from the live schema
// the server-side parse error is the test's fail signal.
func TestClickHouseWriteSmoke(t *testing.T) {
	url := os.Getenv("NEXUS_TEST_CLICKHOUSE_URL")
	if url == "" {
		t.Skip("set NEXUS_TEST_CLICKHOUSE_URL to run the wire smoke")
	}
	ctx := context.Background()
	conn, err := clickhouse.Open(&clickhouse.Options{Addr: []string{url}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()
	if err := conn.Exec(ctx, `TRUNCATE TABLE gateway_traces`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO gateway_traces (
		trace_id, span_id, parent_span_id, timestamp,
		org_id, virtual_key_id,
		operation_name, provider_name, request_model, response_model,
		input_tokens, output_tokens, finish_reason,
		temperature, top_p, max_tokens,
		streamed, ttft_ms, latency_ms, cost_usd,
		status_code, error_type, error_message)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := batch.Append(
		"trace-contract-1", "span-1", "",
		"2026-08-19 12:00:00",
		"org-contract", "vkey-contract",
		"chat", "openai", "gpt-4o", "gpt-4o",
		uint64(100), uint64(50),
		"stop", float32(0.7), float32(0.9), uint64(256),
		false, uint64(150), uint64(800), float32(0.0021),
		uint16(200), "", "",
	); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
	var n uint64
	if err := conn.QueryRow(ctx,
		`SELECT count() FROM gateway_traces WHERE trace_id = ?`,
		"trace-contract-1").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

// silence unused-import lints when build-tag is on.
var _ = strings.ToLower
