// Package limiter provides per-virtual-key rate limiting and monthly spend
// tracking. It has a Redis-backed implementation (shared across gateway
// replicas) and an in-memory fallback for zero-dependency mode.
package limiter

import (
	"context"
	"fmt"
	"time"
)

// Limiter enforces request-rate and budget limits per virtual key.
type Limiter interface {
	// Allow records one request in the current minute window and reports
	// whether the key is still within rpmLimit. rpmLimit <= 0 means unlimited.
	Allow(ctx context.Context, keyID string, rpmLimit int) (bool, error)

	// MonthlySpend returns the current calendar-month spend (USD) for a key.
	MonthlySpend(ctx context.Context, keyID string) (float64, error)

	// AddSpend adds cost (USD) to the current month's running total.
	AddSpend(ctx context.Context, keyID string, costUSD float64) error
}

// minuteWindow returns the current UTC minute bucket key suffix.
func minuteWindow(t time.Time) string {
	return t.UTC().Format("200601021504")
}

// monthWindow returns the current UTC month bucket key suffix.
func monthWindow(t time.Time) string {
	return t.UTC().Format("200601")
}

func rpmKey(keyID, win string) string   { return fmt.Sprintf("nexus:rpm:%s:%s", keyID, win) }
func spendKey(keyID, win string) string { return fmt.Sprintf("nexus:spend:%s:%s", keyID, win) }
