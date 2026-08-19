// Package auditaggregator turns a stream of denial events into a
// more compact row count through a 5-minute burst window. The
// motivation is documented in the package doc on auditaggregator.go.
//
// In practice, the contracts the aggregator exposes are:
//
//  1. AggregatedAction(action) tells the caller whether the event is
//     aggregated or written individually. Policies live here because
//     they belong to the durability/performance side of the audit
//     system, not the routing side.
//
//  2. ResourceFingerprint(target) gives a stable, bounded hex digest
//     of the resource the event targets. Two requests with the same
//     fingerprint fall into the same audit row.
//
//  3. WindowBoundary(now) returns the start time of the aggregation
//     window. SQL uses the start as the dedup key.
//
//  4. The Store.Audit layered on top applies the rules and writes
//     either a row or upserts an existing row's count + last_at.
//
// This file is policy + helpers; the SQL is in Store.AuditDenial.
package auditaggregator

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// AggregatedAction returns true when the action is part of the closed
// set of denial events that get burst-collapsed. Everything else is
// written individually. The closed set is pinned by TestAggregatedSetIsClosed.
func AggregatedAction(a string) bool {
	switch a {
	case "auth.login.denied",
		"user.login.denied",
		"key.rejected.invalid",
		"key.rejected.expired",
		"key.rejected.revoked",
		"rate_limited",
		"request_size",
		"budget.exceeded",
		"concurrency.cap",
		"model.allowlist",
		"invite.rejected.invalid",
		"invite.rejected.expired",
		"invite.rejected.replay":
		return true
	}
	return false
}

// WindowSize is the burst window width. 5 minutes matches minute-
// per-bucket dashboarding and is short enough that the SIEM rule
// "denied_attempts / 1m > N" still fires on the aggregated row.
const WindowSize = 5 * time.Minute

// WindowStart returns the floor-aligned start of the burst window
// containing t. UTC + wall-clock minute (multiple of 5) so replicas
// agree on the boundary.
func WindowStart(t time.Time) time.Time {
	t = t.UTC()
	minute := (t.Minute() / 5) * 5
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), minute, 0, 0, time.UTC)
}

// ResourceFingerprint reduces the request target to a length-bounded hex
// digest. Two events with the same fingerprint collapse into the same row.
// The fingerprint is by design case-insensitive — URLs are case-insensitive
// at the spec level (paths normalised by servers), and attackers cannot
// pretend `/Users` is different from `/users` to inflate the row count.
func ResourceFingerprint(target string) string {
	if target == "" {
		return ""
	}
	target = strings.ToLower(strings.TrimSpace(target))
	if len(target) > 512 {
		target = target[:512]
	}
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:8])
}
