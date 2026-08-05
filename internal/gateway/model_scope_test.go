package gateway

import (
	"context"
	"testing"
)

func TestModelAllowedAcceptsBareUpstreamID(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyAllowedModels, []string{"openai/gpt-4.1-nano"})
	if !modelAllowed(ctx, "gpt-4.1-nano") {
		t.Fatal("bare upstream id should match a scoped hub slug allow-list entry")
	}
	if !modelAllowed(ctx, "openai/gpt-4.1-nano") {
		t.Fatal("exact slug match should still work")
	}
	if modelAllowed(ctx, "gpt-4o-mini") {
		t.Fatal("unrelated model must stay blocked")
	}
}
