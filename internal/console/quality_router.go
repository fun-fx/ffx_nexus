package console

import (
	"context"
	"time"

	"github.com/ffxnexus/nexus/internal/router"
)

// QualityRouterQuerier is the console's narrow view of the running
// router. It exposes three things the admin endpoints need but no
// other caller does:
//
//  1. BlendConfig — the actual weights + decay the router resolves
//     on its next decision. The operator wants "what is the router
//     doing?" to be answerable without lifting the env vars card.
//  2. KnownModels — the lineup the router is currently scoring
//     against. Freshness is reported per-lineup so "we have data
//     for 4 of 7 models" is a single number rather than a puzzle.
//  3. BenchmarkStats — the same view the router blends in. Reading
//     it here means the response carries exactly what the router
//     will use, not a re-projection that may drift.
//
// Keeping this as an interface (rather than depending on
// *router.Router directly) lets PR-2 ship without dragging router
// internals into the console package's test surface.
type QualityRouterQuerier interface {
	BlendConfig() (weights router.CombinedWeights, halfLife time.Duration, bench router.BenchmarkScoreSource)
	KnownModels(ctx context.Context) []string
}

// SetQualityRouter wires the querier. nil is allowed: the quality
// route 503s until a router is available, matching the rest of the
// admin surface that guards optional dependencies.
func (s *Server) SetQualityRouter(q QualityRouterQuerier) {
	s.qualityRouter = q
}
