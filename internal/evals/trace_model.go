package evals

import "github.com/ffxnexus/nexus/internal/observability"

func traceModel(t observability.Trace) string {
	if t.ResponseModel != "" {
		return t.ResponseModel
	}
	return t.RequestModel
}
