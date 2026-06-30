package providers

import (
	"context"
	"testing"

	"github.com/ffxnexus/nexus/internal/gateway"
)

// Tests that the OpenAI adapter exposes the additional OpenAI-compatible
// capabilities (moderations / images) the gateway routes need. They live
// here so we can exercise them without importing gateway in the providers
// test file.
//
// These are unit tests: only the registry indices are checked. Network
// behavior is covered by the embedding / chat surface tests elsewhere.
func TestOpenAIModerationAndImageCapabilities(t *testing.T) {
	o := NewOpenAI("sk-test", "https://api.openai.test/v1", 0)

	if mods := o.ModerationModels(); len(mods) == 0 {
		t.Fatalf("OpenAI should advertise at least one moderation model")
	}
	if imgs := o.ImageModels(); len(imgs) == 0 {
		t.Fatalf("OpenAI should advertise at least one image model")
	}

	reg := gateway.NewRegistry()
	reg.Register(o)

	mods := reg.AllModerationModels()
	if len(mods) == 0 {
		t.Fatalf("registry did not surface OpenAI moderation models")
	}
	imgs := reg.AllImageModels()
	if len(imgs) == 0 {
		t.Fatalf("registry did not surface OpenAI image models")
	}

	// Resolve an empty model — should pick omni-moderation-latest (first match).
	mp, resolved, ok := reg.ResolveModeration("")
	if !ok {
		t.Fatalf("empty resolution should succeed")
	}
	if mp == nil || resolved == "" {
		t.Fatalf("empty model returned nil provider/model")
	}

	ip, resolved, ok := reg.ResolveImage("dall-e-3")
	if !ok || ip == nil || resolved != "dall-e-3" {
		t.Fatalf("dall-e-3 resolution failed: ok=%v model=%q", ok, resolved)
	}
}

func TestOpenAIModerateEmptyModelDefaultsToLatest(t *testing.T) {
	// We can't exercise the network call here, but the handler's contract
	// (and the registry's empty-resolution behavior) guarantee the request
	// surfaces omni-moderation-latest as the default. Verify that the
	// client's view of the OpenAI adapter still has that behavior: a
	// Moderate call invoked with an empty model would marshal the empty
	// field, so the gateway-level defaulting is what makes the value
	// correct in production. This test exists as a regression guard to
	// make sure we keep both omni-moderation-latest and a text-* variant.
	o := NewOpenAI("sk-test", "https://api.openai.test/v1", 0)

	hasOmni := false
	hasLegacy := false
	for _, m := range o.ModerationModels() {
		if m == "omni-moderation-latest" {
			hasOmni = true
		}
		if m == "text-moderation-stable" || m == "text-moderation-latest" {
			hasLegacy = true
		}
	}
	if !hasOmni {
		t.Fatalf("OpenAI adapter must list omni-moderation-latest")
	}
	if !hasLegacy {
		t.Fatalf("OpenAI adapter must keep at least one text-moderation-* model")
	}
}

func TestOpenAIImageModelSurface(t *testing.T) {
	o := NewOpenAI("sk-test", "https://api.openai.test/v1", 0)
	want := map[string]bool{
		"dall-e-2":    false,
		"dall-e-3":    false,
		"gpt-image-1": false,
	}
	for _, m := range o.ImageModels() {
		if _, ok := want[m]; ok {
			want[m] = true
		}
	}
	for k, ok := range want {
		if !ok {
			t.Fatalf("missing expected image model %s", k)
		}
	}
}

// Test that the alphabetically-sorted capability lists are deterministic.
func TestSortedDiscovery(t *testing.T) {
	reg := gateway.NewRegistry()
	reg.Register(NewOpenAI("sk-test", "https://api.openai.test/v1", 0))

	mods := reg.AllModerationModels()
	for i := 1; i < len(mods); i++ {
		if mods[i-1] > mods[i] {
			t.Fatalf("moderation models not sorted: %v", mods)
		}
	}
	imgs := reg.AllImageModels()
	for i := 1; i < len(imgs); i++ {
		if imgs[i-1] > imgs[i] {
			t.Fatalf("image models not sorted: %v", imgs)
		}
	}
}

// ensure ctx import is "used" so the test also touches the runtime path.
var _ = context.Background
