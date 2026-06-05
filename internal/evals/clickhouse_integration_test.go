package evals

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/ffxnexus/nexus/internal/observability"
)

func chDSN() string {
	if d := os.Getenv("NEXUS_CLICKHOUSE_URL"); d != "" {
		return d
	}
	return "clickhouse://nexus:nexus@localhost:9000/nexus"
}

func openClickHouse(t *testing.T) driver.Conn {
	t.Helper()
	opts, err := clickhouse.ParseDSN(chDSN())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		t.Skipf("clickhouse not reachable at %s: %v", chDSN(), err)
	}
	return conn
}

// End-to-end: Worker -> RemoteEvaluator (HTTP) -> CHSink -> ClickHouse row.
func TestRemoteEvalPersistsToClickHouse(t *testing.T) {
	conn := openClickHouse(t)

	traceID := fmt.Sprintf("it-remote-%d", time.Now().UnixNano())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"scores":[
			{"evaluator":"deepeval","metric":"answer_relevancy","score":0.91,"passed":true,"rationale":"relevant","judge_model":"fake"},
			{"evaluator":"deepeval","metric":"toxicity","score":0.04,"passed":true,"rationale":"clean","judge_model":"fake"}
		]}`))
	}))
	defer srv.Close()

	remote := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL, Metrics: []string{"answer_relevancy", "toxicity"}})
	w := NewWorker(Options{
		Judges:          []Evaluator{remote},
		Sink:            NewCHSink(conn),
		JudgeSampleRate: 1.0,
		Workers:         2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	w.Record(observability.Trace{
		TraceID:        traceID,
		StatusCode:     200,
		RequestModel:   "gemini-2.5-flash",
		InputMessages:  `[{"role":"user","content":"2+2?"}]`,
		OutputMessages: "4",
	})

	_ = w.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var count uint64
	for {
		row := conn.QueryRow(ctx, `SELECT count() FROM eval_scores WHERE trace_id = ? AND evaluator = 'deepeval'`, traceID)
		if err := row.Scan(&count); err != nil {
			t.Fatalf("query: %v", err)
		}
		if count >= 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for deepeval rows (got %d)", count)
		case <-time.After(200 * time.Millisecond):
		}
	}

	var metric string
	var score float64
	row := conn.QueryRow(ctx, `
		SELECT metric, score FROM eval_scores
		WHERE trace_id = ? AND metric = 'answer_relevancy'
		ORDER BY timestamp DESC LIMIT 1`, traceID)
	if err := row.Scan(&metric, &score); err != nil {
		t.Fatalf("fetch row: %v", err)
	}
	if metric != "answer_relevancy" || score != 0.91 {
		t.Fatalf("unexpected row: metric=%q score=%v", metric, score)
	}

	// Cleanup test rows (best-effort).
	_ = conn.Exec(ctx, `ALTER TABLE eval_scores DELETE WHERE trace_id = ?`, traceID)
}

func TestRemoteEvalPersistsRAGScoresToClickHouse(t *testing.T) {
	conn := openClickHouse(t)

	traceID := fmt.Sprintf("it-rag-%d", time.Now().UnixNano())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"scores":[
			{"evaluator":"ragas","metric":"ragas_faithfulness","score":0.87,"passed":true,"judge_model":"fake"}
		]}`))
	}))
	defer srv.Close()

	remote := NewRemoteEvaluator(RemoteConfig{BaseURL: srv.URL, Metrics: []string{"answer_relevancy"}})
	w := NewWorker(Options{
		Judges:          []Evaluator{remote},
		Sink:            NewCHSink(conn),
		JudgeSampleRate: 1.0,
		Workers:         2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	w.Record(observability.Trace{
		TraceID:           traceID,
		StatusCode:        200,
		RequestModel:      "gemini-2.5-flash",
		InputMessages:     `[{"role":"user","content":"Capital of France?"}]`,
		OutputMessages:    "Paris",
		RetrievalContexts: `["Paris is the capital of France."]`,
		EvalReference:     "Paris",
	})

	_ = w.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var metric string
	for {
		row := conn.QueryRow(ctx, `
			SELECT metric FROM eval_scores
			WHERE trace_id = ? AND metric = 'ragas_faithfulness'`, traceID)
		if err := row.Scan(&metric); err == nil && metric == "ragas_faithfulness" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for ragas_faithfulness row")
		case <-time.After(200 * time.Millisecond):
		}
	}

	_ = conn.Exec(ctx, `ALTER TABLE eval_scores DELETE WHERE trace_id = ?`, traceID)
}
