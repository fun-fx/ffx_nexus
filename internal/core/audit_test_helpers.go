package core

// Test-only helper: seed a request id into the context using the same key
// the resp middleware uses, so audit rows written from this test can be
// joined on the same id the response would carry. Mirrors resp.WithRequestID
// but is local to the test package — keeps import cycles out of core.

import (
	"context"

	"github.com/ffxnexus/nexus/internal/resp"
)

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, resp.RequestIDKey(), id)
}
