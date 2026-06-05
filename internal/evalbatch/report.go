package evalbatch

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// MetricSummary aggregates one metric across all evaluated cases.
type MetricSummary struct {
	Metric    string  `json:"metric"`
	Count     int     `json:"count"`
	MeanScore float64 `json:"mean_score"`
	MinScore  float64 `json:"min_score"`
	MaxScore  float64 `json:"max_score"`
	PassRate  float64 `json:"pass_rate"`
}

// Report is the full result of a batch run, suitable for JSON serialization and
// use as a future baseline.
type Report struct {
	GeneratedAt time.Time                `json:"generated_at"`
	Dataset     string                   `json:"dataset,omitempty"`
	NumCases    int                      `json:"num_cases"`
	NumErrors   int                      `json:"num_errors"`
	Metrics     map[string]MetricSummary `json:"metrics"`
	Cases       []CaseResult             `json:"cases,omitempty"`
}

// BuildReport aggregates case results into per-metric summaries. When
// includeCases is false, the per-case detail is omitted from the report.
func BuildReport(dataset string, results []CaseResult, includeCases bool) *Report {
	type acc struct {
		count         int
		sum, min, max float64
		passes        int
	}
	by := map[string]*acc{}
	errors := 0

	for _, r := range results {
		if r.Error != "" {
			errors++
		}
		for _, s := range r.Scores {
			a := by[s.Metric]
			if a == nil {
				a = &acc{min: s.Score, max: s.Score}
				by[s.Metric] = a
			}
			a.count++
			a.sum += s.Score
			if s.Score < a.min {
				a.min = s.Score
			}
			if s.Score > a.max {
				a.max = s.Score
			}
			if s.Passed {
				a.passes++
			}
		}
	}

	metrics := make(map[string]MetricSummary, len(by))
	for m, a := range by {
		metrics[m] = MetricSummary{
			Metric:    m,
			Count:     a.count,
			MeanScore: a.sum / float64(a.count),
			MinScore:  a.min,
			MaxScore:  a.max,
			PassRate:  float64(a.passes) / float64(a.count),
		}
	}

	rep := &Report{
		GeneratedAt: time.Now().UTC(),
		Dataset:     dataset,
		NumCases:    len(results),
		NumErrors:   errors,
		Metrics:     metrics,
	}
	if includeCases {
		rep.Cases = results
	}
	return rep
}

// Regression describes one metric whose mean score dropped versus the baseline
// by more than the allowed tolerance.
type Regression struct {
	Metric       string  `json:"metric"`
	BaselineMean float64 `json:"baseline_mean"`
	CurrentMean  float64 `json:"current_mean"`
	Delta        float64 `json:"delta"` // current - baseline (negative = worse)
}

// CompareBaseline returns regressions where a metric present in the baseline
// dropped by more than tolerance. Metrics absent from the current run are
// reported as a full drop to 0. Metrics new in the current run are ignored.
func (rep *Report) CompareBaseline(baseline *Report, tolerance float64) []Regression {
	if baseline == nil {
		return nil
	}
	var regs []Regression
	for m, base := range baseline.Metrics {
		cur, ok := rep.Metrics[m]
		curMean := 0.0
		if ok {
			curMean = cur.MeanScore
		}
		delta := curMean - base.MeanScore
		if delta < -tolerance {
			regs = append(regs, Regression{
				Metric:       m,
				BaselineMean: base.MeanScore,
				CurrentMean:  curMean,
				Delta:        delta,
			})
		}
	}
	sort.Slice(regs, func(i, j int) bool { return regs[i].Metric < regs[j].Metric })
	return regs
}

// LoadReport reads a previously written report (used as a baseline).
func LoadReport(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rep Report
	if err := json.NewDecoder(f).Decode(&rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// WriteJSON writes the report as indented JSON.
func (rep *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// RenderText writes a human-readable summary table.
func (rep *Report) RenderText(w io.Writer) {
	fmt.Fprintf(w, "Dataset: %s\n", rep.Dataset)
	fmt.Fprintf(w, "Cases:   %d (%d errors)\n\n", rep.NumCases, rep.NumErrors)

	names := make([]string, 0, len(rep.Metrics))
	for m := range rep.Metrics {
		names = append(names, m)
	}
	sort.Strings(names)

	fmt.Fprintf(w, "%-26s %6s %10s %10s %10s\n", "METRIC", "N", "MEAN", "PASS%", "MIN/MAX")
	fmt.Fprintf(w, "%s\n", strings.Repeat("-", 70))
	for _, m := range names {
		s := rep.Metrics[m]
		fmt.Fprintf(w, "%-26s %6d %10.3f %9.1f%% %4.2f/%-4.2f\n",
			s.Metric, s.Count, s.MeanScore, s.PassRate*100, s.MinScore, s.MaxScore)
	}
}
