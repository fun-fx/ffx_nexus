package benchmark

import "testing"

func TestAllowedModelsForBenchmarkIncludesBareID(t *testing.T) {
	got := allowedModelsForBenchmark("openai/gpt-4.1-nano")
	if len(got) != 2 || got[0] != "openai/gpt-4.1-nano" || got[1] != "gpt-4.1-nano" {
		t.Fatalf("allowed models = %#v", got)
	}
	if models := allowedModelsForBenchmark("gemini-2.5-flash"); len(models) != 1 || models[0] != "gemini-2.5-flash" {
		t.Fatalf("unprefixed model = %#v", models)
	}
}
