package semcache

import (
	"context"
	"strings"
	"testing"
)

// The most severe leak the semantic cache could produce: org B asks a question,
// and receives the completion Nexus generated for org A.
//
// The cache key is scoped today, but nothing asserted it, and the property is one
// line away from being lost — a refactor that moved the model into the key and
// dropped the scope, or a "share the cache across the installation to raise the
// hit rate" optimisation, would both look reasonable in review. The consequence
// is not a wrong answer: it is one department reading another department's
// generated content, which is the worst outcome in the product.
//
// The assertion is on what a Lookup RETURNS, not on the shape of the key string.
// A test that checked the key format would pass on an implementation that built a
// scoped key and then ignored it.

// identicalVectors makes every prompt semantically identical, so similarity can
// never be the reason a lookup misses. If org B does not get org A's entry here,
// the only possible reason is the scope.
var identicalVectors = []float32{1, 0, 0}

func tenancyCache(t *testing.T) *Memory {
	t.Helper()
	return NewMemory(Config{Threshold: 0.99, MaxEntriesPerModel: 100})
}

func TestOneOrgNeverReceivesAnotherOrgsCachedCompletion(t *testing.T) {
	cache := tenancyCache(t)
	ctx := context.Background()
	const model = "gpt-4o"
	orgASecret := []byte(`{"choices":[{"message":{"content":"ORG-A-CONFIDENTIAL-ANSWER"}}]}`)

	if err := cache.Store(ctx, "org-a", model, "what is our Q3 revenue?", identicalVectors, orgASecret); err != nil {
		t.Fatalf("store for org A: %v", err)
	}

	// Org B asks a semantically identical question.
	hit, err := cache.Lookup(ctx, "org-b", model, "what is our Q3 revenue?", identicalVectors)
	if err != nil {
		t.Fatalf("lookup for org B: %v", err)
	}
	if hit != nil {
		t.Fatalf("org B received a cache hit created by org A.\n"+
			"This returns one org's generated content to another org verbatim, which "+
			"is the most severe disclosure the cache can produce.\nresponse: %s",
			string(hit.ResponseJSON))
	}

	// Non-vacuity: org A must hit its own entry. Without this the test would pass
	// on a cache that never returns anything at all.
	own, err := cache.Lookup(ctx, "org-a", model, "what is our Q3 revenue?", identicalVectors)
	if err != nil {
		t.Fatalf("lookup for org A: %v", err)
	}
	if own == nil {
		t.Fatal("org A did not hit its own entry, so the negative assertion above " +
			"proves nothing about scoping — the cache may simply be inert")
	}
	if !strings.Contains(string(own.ResponseJSON), "ORG-A-CONFIDENTIAL-ANSWER") {
		t.Errorf("org A's own hit did not return org A's response: %s", own.ResponseJSON)
	}
}

// The same property in the other direction: two orgs storing under the same model
// and prompt must not overwrite each other, or the second writer's org would
// receive the first's response after an eviction reordering.
func TestTwoOrgsStoringTheSamePromptKeepSeparateEntries(t *testing.T) {
	cache := tenancyCache(t)
	ctx := context.Background()
	const model = "gpt-4o"

	if err := cache.Store(ctx, "org-a", model, "same question", identicalVectors,
		[]byte(`{"answer":"A"}`)); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(ctx, "org-b", model, "same question", identicalVectors,
		[]byte(`{"answer":"B"}`)); err != nil {
		t.Fatal(err)
	}

	for org, want := range map[string]string{"org-a": `"A"`, "org-b": `"B"`} {
		hit, err := cache.Lookup(ctx, org, model, "same question", identicalVectors)
		if err != nil {
			t.Fatalf("%s: lookup: %v", org, err)
		}
		if hit == nil {
			t.Errorf("%s: no hit for its own entry", org)
			continue
		}
		if !strings.Contains(string(hit.ResponseJSON), want) {
			t.Errorf("%s received %s, want the entry containing %s. The two orgs' "+
				"entries collided.", org, hit.ResponseJSON, want)
		}
	}
}

// The scope must be part of the key, not merely present in it. An implementation
// that concatenated scope and model without a separator would let scope "org" +
// model "a:m" collide with scope "org:a" + model "m".
func TestScopeAndModelCannotCollideThroughConcatenation(t *testing.T) {
	cache := tenancyCache(t)
	ctx := context.Background()

	if err := cache.Store(ctx, "org", "a:gpt-4o", "q", identicalVectors,
		[]byte(`{"answer":"first"}`)); err != nil {
		t.Fatal(err)
	}
	hit, err := cache.Lookup(ctx, "org:a", "gpt-4o", "q", identicalVectors)
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Errorf("scope %q + model %q collided with scope %q + model %q: %s\n"+
			"A separator-free key lets a crafted org or model name read another "+
			"scope's entries.", "org", "a:gpt-4o", "org:a", "gpt-4o", hit.ResponseJSON)
	}
}

// Different models within one org must also stay separate, or a cheap model would
// serve a premium model's answers and the cost accounting would be wrong too.
func TestEntriesAreSeparatedByModelWithinAnOrg(t *testing.T) {
	cache := tenancyCache(t)
	ctx := context.Background()

	if err := cache.Store(ctx, "org-a", "gpt-4o", "q", identicalVectors,
		[]byte(`{"answer":"expensive"}`)); err != nil {
		t.Fatal(err)
	}
	hit, err := cache.Lookup(ctx, "org-a", "gpt-4o-mini", "q", identicalVectors)
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Errorf("a lookup for gpt-4o-mini returned a gpt-4o entry: %s", hit.ResponseJSON)
	}
}
