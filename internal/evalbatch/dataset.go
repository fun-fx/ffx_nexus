// Package evalbatch runs an offline regression-evaluation pass over a fixed
// dataset of cases. Each case is scored by an evals.Evaluator (typically the
// RemoteEvaluator backed by the Python eval sidecar), the scores are aggregated
// per metric, and the run can be compared against a stored baseline to detect
// quality regressions in CI.
//
// Unlike the online eval Worker, this path is synchronous and deterministic:
// every case is evaluated (no sampling) and the caller controls concurrency.
package evalbatch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Message mirrors an OpenAI chat message for cases that need multi-turn input.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Case is a single regression-eval record. The dataset is JSON Lines: one Case
// per line. Either Input (a single user prompt) or Messages must be provided.
// Output may be pre-recorded; when empty the caller may generate it via the
// gateway before evaluation.
type Case struct {
	ID        string    `json:"id"`
	Model     string    `json:"model,omitempty"`
	Input     string    `json:"input,omitempty"`
	Messages  []Message `json:"messages,omitempty"`
	Output    string    `json:"output,omitempty"`
	Reference string    `json:"reference,omitempty"`
	Contexts  []string  `json:"contexts,omitempty"`
}

// Messages returns the chat messages for the case, deriving a single user
// message from Input when Messages is not set.
func (c Case) ChatMessages() []Message {
	if len(c.Messages) > 0 {
		return c.Messages
	}
	if c.Input != "" {
		return []Message{{Role: "user", Content: c.Input}}
	}
	return nil
}

// LoadDataset reads a JSON Lines dataset from path. Blank lines and lines
// starting with '#' are ignored. Each remaining line must be a valid Case with
// either Input or Messages set. IDs are auto-assigned (case-N) when missing.
func LoadDataset(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseDataset(f)
}

func parseDataset(r io.Reader) ([]Case, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // allow long lines
	var cases []Case
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", line, err)
		}
		if c.Input == "" && len(c.Messages) == 0 {
			return nil, fmt.Errorf("dataset line %d: case must set input or messages", line)
		}
		if c.ID == "" {
			c.ID = fmt.Sprintf("case-%d", len(cases)+1)
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("dataset is empty")
	}
	return cases, nil
}
