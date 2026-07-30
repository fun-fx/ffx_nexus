package external

import (
	"strings"
	"testing"
	"time"

	"github.com/ffxnexus/nexus/internal/observability"
)

// redactWithin runs the redaction on its own goroutine so a
// non-terminating implementation fails the test instead of hanging the
// package until the go test timeout kills it.
func redactWithin(t *testing.T, in string, budget time.Duration) string {
	t.Helper()
	type result struct{ out string }
	ch := make(chan result, 1)
	go func() { ch <- result{redactPII(in)} }()
	select {
	case r := <-ch:
		return r.out
	case <-time.After(budget):
		t.Fatalf("redactPII did not return within %s for input of %d bytes", budget, len(in))
		return ""
	}
}

// TestRedactPIITerminatesOnProseAtSign is the regression that cost a
// week of debugging: a prompt that mentions '@' as prose rather than as
// an address made the old scanner spin at full CPU forever inside the
// eval worker, so traces silently stopped reaching the plugin's vendor
// with no error anywhere. This exact sentence ships in a widely used
// agent system prompt.
func TestRedactPIITerminatesOnProseAtSign(t *testing.T) {
	in := "Users can reference context like files and folders using the @ symbol, " +
		"e.g. @src/components/ is a reference to the src/components/ folder."
	out := redactWithin(t, in, 2*time.Second)
	if out != in {
		t.Fatalf("a bare '@' is not an address and must be left alone:\n got: %s\nwant: %s", out, in)
	}
}

// TestRedactPIITerminatesOnMixedAtSigns pairs a real address with prose
// '@' usage, which is what made the input and output indices diverge.
func TestRedactPIITerminatesOnMixedAtSigns(t *testing.T) {
	in := "mail me@example.com then @mention " + strings.Repeat("filler ", 200)
	out := redactWithin(t, in, 2*time.Second)
	if strings.Contains(out, "me@example.com") {
		t.Fatal("the address was not masked")
	}
	if !strings.Contains(out, "@mention") {
		t.Fatalf("prose '@' must survive: %s", out[:80])
	}
}

// TestRedactPIIHandlesLargePayload keeps the pass linear: the prompts
// that triggered the hang in production were tens to hundreds of KB.
func TestRedactPIIHandlesLargePayload(t *testing.T) {
	in := strings.Repeat("see the @ symbol and mail a@b.com plus filler text ", 8000)
	out := redactWithin(t, in, 5*time.Second)
	if strings.Contains(out, "a@b.com") {
		t.Fatal("addresses in a large payload were not masked")
	}
}

func TestRedactPIIMasksEveryAddress(t *testing.T) {
	in := "first a@b.com second c.d+tag@example.co.uk third nope@localhost"
	out := redactWithin(t, in, 2*time.Second)
	for _, masked := range []string{"a@b.com", "c.d+tag@example.co.uk"} {
		if strings.Contains(out, masked) {
			t.Fatalf("%s was not masked: %s", masked, out)
		}
	}
	// No dotted TLD, so it stays — the pattern is deliberately
	// conservative and matches the PII evaluator's notion of an address.
	if !strings.Contains(out, "nope@localhost") {
		t.Fatalf("conservative pattern should skip bare hosts: %s", out)
	}
}

// TestRedactPayloadMasksStringValues covers the caller: only string
// fields are rewritten, and non-pii redaction kinds are a no-op.
func TestRedactPayloadMasksStringValues(t *testing.T) {
	payload := map[string]any{
		"input":  "reach a@b.com via the @ symbol",
		"tokens": 42,
	}
	if err := redactPayload(payload, observability.Trace{}, []string{"pii"}); err != nil {
		t.Fatalf("redactPayload: %v", err)
	}
	if got := payload["input"].(string); strings.Contains(got, "a@b.com") {
		t.Fatalf("address survived redaction: %s", got)
	}
	if payload["tokens"] != 42 {
		t.Fatal("non-string values must be left untouched")
	}

	untouched := map[string]any{"input": "reach a@b.com"}
	if err := redactPayload(untouched, observability.Trace{}, []string{"none"}); err != nil {
		t.Fatalf("redactPayload: %v", err)
	}
	if untouched["input"] != "reach a@b.com" {
		t.Fatal("redaction ran without the pii kind requested")
	}
}
