package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeLimiter struct {
	allow bool
	spend float64
}

func (f *fakeLimiter) Allow(context.Context, string, int) (bool, error) {
	return f.allow, nil
}
func (f *fakeLimiter) MonthlySpend(context.Context, string) (float64, error) {
	return f.spend, nil
}
func (f *fakeLimiter) AddSpend(context.Context, string, float64) error { return nil }

func runEnforce(t *testing.T, lim Limiter, ctx context.Context) int {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	Enforce(lim)(next).ServeHTTP(rec, req)
	return rec.Code
}

func authedCtx(vkeyID string, rpm int, budget float64) context.Context {
	ctx := context.WithValue(context.Background(), ctxKeyVKeyID, vkeyID)
	ctx = context.WithValue(ctx, ctxKeyRPMLimit, rpm)
	ctx = context.WithValue(ctx, ctxKeyMonthlyBudget, budget)
	return ctx
}

func TestEnforceRateLimit429(t *testing.T) {
	lim := &fakeLimiter{allow: false}
	if code := runEnforce(t, lim, authedCtx("vk1", 10, 0)); code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", code)
	}
}

func TestEnforceBudget402(t *testing.T) {
	lim := &fakeLimiter{allow: true, spend: 100}
	if code := runEnforce(t, lim, authedCtx("vk1", 10, 50)); code != http.StatusPaymentRequired {
		t.Fatalf("want 402, got %d", code)
	}
}

func TestEnforceAllowed(t *testing.T) {
	lim := &fakeLimiter{allow: true, spend: 10}
	if code := runEnforce(t, lim, authedCtx("vk1", 10, 50)); code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
}

func TestEnforceUnauthenticatedPassthrough(t *testing.T) {
	// No vkey in context (zero-dependency mode): enforcement is skipped.
	lim := &fakeLimiter{allow: false, spend: 9999}
	if code := runEnforce(t, lim, context.Background()); code != http.StatusOK {
		t.Fatalf("want 200 passthrough, got %d", code)
	}
}

func TestEnforceNilLimiter(t *testing.T) {
	if code := runEnforce(t, nil, authedCtx("vk1", 1, 1)); code != http.StatusOK {
		t.Fatalf("want 200 with nil limiter, got %d", code)
	}
}
