package evals

// EvalOverride carries per-batch overrides the worker injects in the
// /evaluate /evaluate/batch call so a profile-bound judge/embeddings
// endpoint can wake up at runtime. PR #136 introduces this so a
// profile PATCH on console immediately flows through to the next
// /evaluate call without restart.
//
// All fields are zero-value safe; the JSON omitempty keeps single-
// trace /evaluate callers unchanged when nothing was overridden.
type EvalOverride struct {
	JudgeURL   string  `json:"judge_url,omitempty"`
	JudgeModel string  `json:"judge_model,omitempty"`
	Embeddings string  `json:"embeddings_url,omitempty"`
	Threshold  float64 `json:"threshold,omitempty"`
}

// HasOverride reports whether any non-zero field is set. Used by the
// dispatch path to decide whether to call /evaluate/batch or fall
// back to /evaluate single-trace with no override.
func (o *EvalOverride) HasOverride() bool {
	if o == nil {
		return false
	}
	return o.JudgeURL != "" || o.JudgeModel != "" || o.Embeddings != "" || o.Threshold > 0
}

// overrideFromProfile returns the override payload for a single
// profile. Only populated when the kind is non-heuristic, since
// heuristics never call out.
func overrideFromProfile(p EvalProfile) *EvalOverride {
	if p.Kind == ProfileHeuristicPII || p.Kind == ProfileHeuristicCompleteness {
		return nil
	}
	return &EvalOverride{
		JudgeURL:   p.Endpoint.BaseURL,
		JudgeModel: p.Endpoint.Model,
		Threshold:  p.Threshold,
	}
}
