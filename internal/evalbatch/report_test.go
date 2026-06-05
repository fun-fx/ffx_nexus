package evalbatch

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ffxnexus/nexus/internal/evals"
)

func sampleResults() []CaseResult {
	return []CaseResult{
		{ID: "a", Scores: []evals.Score{
			{Metric: "answer_relevancy", Score: 0.8, Passed: true},
			{Metric: "toxicity", Score: 0.2, Passed: true},
		}},
		{ID: "b", Scores: []evals.Score{
			{Metric: "answer_relevancy", Score: 0.6, Passed: false},
			{Metric: "toxicity", Score: 0.4, Passed: true},
		}},
		{ID: "c", Error: "boom"},
	}
}

func TestBuildReportAggregates(t *testing.T) {
	rep := BuildReport("ds.jsonl", sampleResults(), false)

	if rep.NumCases != 3 {
		t.Errorf("want 3 cases, got %d", rep.NumCases)
	}
	if rep.NumErrors != 1 {
		t.Errorf("want 1 error, got %d", rep.NumErrors)
	}
	ar := rep.Metrics["answer_relevancy"]
	if ar.Count != 2 {
		t.Errorf("want count 2, got %d", ar.Count)
	}
	if ar.MeanScore < 0.699 || ar.MeanScore > 0.701 {
		t.Errorf("want mean ~0.7, got %f", ar.MeanScore)
	}
	if ar.PassRate != 0.5 {
		t.Errorf("want pass rate 0.5, got %f", ar.PassRate)
	}
	if ar.MinScore != 0.6 || ar.MaxScore != 0.8 {
		t.Errorf("want min/max 0.6/0.8, got %f/%f", ar.MinScore, ar.MaxScore)
	}
	if rep.Cases != nil {
		t.Errorf("includeCases=false should omit cases")
	}
}

func TestBuildReportIncludesCases(t *testing.T) {
	rep := BuildReport("ds", sampleResults(), true)
	if len(rep.Cases) != 3 {
		t.Errorf("want cases included, got %d", len(rep.Cases))
	}
}

func TestCompareBaselineDetectsRegression(t *testing.T) {
	base := BuildReport("ds", []CaseResult{
		{ID: "a", Scores: []evals.Score{{Metric: "answer_relevancy", Score: 0.9, Passed: true}}},
	}, false)
	cur := BuildReport("ds", []CaseResult{
		{ID: "a", Scores: []evals.Score{{Metric: "answer_relevancy", Score: 0.7, Passed: true}}},
	}, false)

	regs := cur.CompareBaseline(base, 0.05)
	if len(regs) != 1 {
		t.Fatalf("want 1 regression, got %d", len(regs))
	}
	if regs[0].Metric != "answer_relevancy" {
		t.Errorf("unexpected metric: %s", regs[0].Metric)
	}
	if regs[0].Delta > -0.19 {
		t.Errorf("want delta ~-0.2, got %f", regs[0].Delta)
	}
}

func TestCompareBaselineWithinTolerance(t *testing.T) {
	base := BuildReport("ds", []CaseResult{
		{ID: "a", Scores: []evals.Score{{Metric: "answer_relevancy", Score: 0.80, Passed: true}}},
	}, false)
	cur := BuildReport("ds", []CaseResult{
		{ID: "a", Scores: []evals.Score{{Metric: "answer_relevancy", Score: 0.78, Passed: true}}},
	}, false)

	if regs := cur.CompareBaseline(base, 0.05); len(regs) != 0 {
		t.Fatalf("small drop within tolerance should not regress, got %+v", regs)
	}
}

func TestCompareBaselineMissingMetric(t *testing.T) {
	base := BuildReport("ds", []CaseResult{
		{ID: "a", Scores: []evals.Score{{Metric: "toxicity", Score: 0.9, Passed: true}}},
	}, false)
	cur := BuildReport("ds", []CaseResult{
		{ID: "a", Scores: []evals.Score{{Metric: "answer_relevancy", Score: 0.9, Passed: true}}},
	}, false)

	regs := cur.CompareBaseline(base, 0.05)
	if len(regs) != 1 || regs[0].Metric != "toxicity" {
		t.Fatalf("missing metric should regress to 0, got %+v", regs)
	}
}

func TestCompareBaselineNil(t *testing.T) {
	cur := BuildReport("ds", sampleResults(), false)
	if regs := cur.CompareBaseline(nil, 0.05); regs != nil {
		t.Fatalf("nil baseline should yield no regressions")
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	rep := BuildReport("ds", sampleResults(), false)
	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	// LoadReport reads from a file; here just verify decode via the same shape.
	var out Report
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Metrics["toxicity"].Count != 2 {
		t.Errorf("round-trip lost data: %+v", out.Metrics)
	}
}

func TestRenderTextRuns(t *testing.T) {
	rep := BuildReport("ds", sampleResults(), false)
	var buf bytes.Buffer
	rep.RenderText(&buf)
	if buf.Len() == 0 {
		t.Fatal("expected rendered output")
	}
}
