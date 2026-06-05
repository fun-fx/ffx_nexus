package evals

import (
	"encoding/json"
	"strings"
)

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// extractPrompt returns the user question text from a JSON messages array.
// Falls back to the raw string when parsing fails.
func extractPrompt(inputMessagesJSON string) string {
	var msgs []chatMsg
	if err := json.Unmarshal([]byte(inputMessagesJSON), &msgs); err != nil {
		return inputMessagesJSON
	}
	// Prefer the last user message (typical RAG query).
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	var b strings.Builder
	for i, m := range msgs {
		if m.Content == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Content)
		if i == len(msgs)-1 {
			break
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	return inputMessagesJSON
}

// parseRetrievalContexts decodes the JSON array stored on Trace.RetrievalContexts.
func parseRetrievalContexts(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// contextMetrics are requested automatically when retrieval contexts are present.
var contextMetrics = []string{"hallucination", "ragas_faithfulness"}

func mergeContextMetrics(base []string, contexts []string) []string {
	if len(contexts) == 0 {
		return base
	}
	out := append([]string(nil), base...)
	for _, m := range contextMetrics {
		if !containsMetric(out, m) {
			out = append(out, m)
		}
	}
	return out
}

func containsMetric(metrics []string, want string) bool {
	for _, m := range metrics {
		if m == want {
			return true
		}
	}
	return false
}
