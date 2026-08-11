package main

import (
	"context"
	"time"

	"github.com/ffxnexus/nexus/internal/router"
)

// routerQualityQuerier is the bridge between the gateway's
// *router.Router (whose Source may be a *router.CombinedProvider
// when bench blending is wired) and the console's
// QualityRouterQuerier. We read the combined provider when present
// so the operator's question is "what does the router blend with"
// — the underlying primary's response would skip the bench layer
// entirely on a configured routing build.
//
// When the router's source is not a CombinedProvider (an older
// wiring without the bench layer), the querier reports
// BenchmarkWeight = 0 and a nil source. The console's handler
// treats that as "bench blend inactive / unsupported", which is
// honest.
type routerQualityQuerier struct {
	router   *router.Router
	combined *router.CombinedProvider
}

// NewRouterQualityQuerier builds the querier. A nil router means
// the console's quality route will 503; the wiring has nothing else
// to fail to.
//
// When the router is not backed by a CombinedProvider, we still
// return a non-nil querier so the route answers 200 with the
// "blend inactive" view — surfacing that the bench blend is not
// configured is itself useful to the operator.
func NewRouterQualityQuerier(r *router.Router) *routerQualityQuerier {
	if r == nil {
		return nil
	}
	src := r.Source()
	cp, _ := src.(*router.CombinedProvider)
	return &routerQualityQuerier{router: r, combined: cp}
}

// BlendConfig returns (weights, half-life, source) as the router
// will use them on the next decision. We do NOT cache this — the
// router re-resolves on each request and the operator's mental
// model is "what is the router doing right now", not "what the
// router was doing at last restart".
func (q *routerQualityQuerier) BlendConfig() (router.CombinedWeights, time.Duration, router.BenchmarkScoreSource) {
	if q == nil || q.combined == nil {
		return router.CombinedWeights{}, 0, nil
	}
	return q.combined.Weights(), q.combined.HalfLife(), q.combined.BenchSource()
}

// KnownModels is delegated to the router, which exposes the live
// cache. For a deployment that has not yet refreshed, the returned
// list is empty (the right answer for an "I have no signal yet"
// state).
func (q *routerQualityQuerier) KnownModels(_ context.Context) []string {
	if q == nil || q.router == nil {
		return nil
	}
	return q.router.KnownModels()
}
