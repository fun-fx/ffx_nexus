package observability

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TraceOrgLookup resolves a trace id to the organisation that owns it.
//
// It exists for the eval-plugin collect path. An external evaluator pushes a
// score back over a webhook (unauthenticated inbound POST) or is polled on a
// background tick; neither carries a Nexus session, so the only tenant signal
// available is the trace_id the vendor echoes. Without this lookup every score
// from a cluster-wide plugin had to be written to one fixed org, which in a
// multi-department installation means one department's vendor results appear in
// another's quality dashboard.
//
// Results are cached because vendors fan a burst of scores out per trace (one
// per metric) and the same trace_id then arrives several times within seconds.
// The cache is bounded and short-lived: attribution must not go stale across a
// deployment, and an unbounded map fed by an externally-supplied key is a
// memory-growth vector reachable by anyone who can reach the webhook.
type TraceOrgLookup struct {
	conn driver.Conn

	mu    sync.Mutex
	cache map[string]orgCacheEntry
}

type orgCacheEntry struct {
	org       string
	found     bool
	expiresAt time.Time
}

const (
	// traceOrgCacheTTL is short on purpose. A wrong attribution cached for
	// hours is worse than a second ClickHouse point-lookup, which reads one
	// row by the table's primary key.
	traceOrgCacheTTL = 60 * time.Second
	// traceOrgCacheMax bounds the map. The key is attacker-supplied (any
	// trace id in a webhook body), so the cache is cleared wholesale once it
	// grows past this rather than evicted entry-by-entry — the cheap policy
	// is adequate for a lookup whose miss cost is one indexed read.
	traceOrgCacheMax = 4096
)

// NewTraceOrgLookup builds a lookup over the recorder's ClickHouse connection.
func (r *CHRecorder) NewTraceOrgLookup() *TraceOrgLookup {
	return &TraceOrgLookup{conn: r.conn, cache: make(map[string]orgCacheEntry)}
}

// OrgForTrace returns the org owning traceID, and false when the trace is
// unknown or the lookup fails.
//
// A failed query and an unknown trace deliberately return the same thing. The
// caller's next decision is "attribute or refuse", and a ClickHouse timeout is
// no more of a licence to guess than a forged trace id is.
func (l *TraceOrgLookup) OrgForTrace(ctx context.Context, traceID string) (string, bool) {
	traceID = strings.TrimSpace(traceID)
	if traceID == "" || l == nil || l.conn == nil {
		return "", false
	}
	if org, found, ok := l.cached(traceID); ok {
		return org, found
	}

	// A point lookup on the sorting key. LIMIT 1 because a trace id can carry
	// several spans and they necessarily share an org.
	var org string
	row := l.conn.QueryRow(ctx, `
		SELECT org_id FROM gateway_traces
		WHERE trace_id = ?
		LIMIT 1`, traceID)
	err := row.Scan(&org)
	switch {
	case err == nil:
		org = strings.TrimSpace(org)
		l.store(traceID, org, org != "")
		return org, org != ""
	case isNoRows(err):
		l.store(traceID, "", false)
		return "", false
	default:
		// Not cached: a transient failure must not pin "unknown" for a minute.
		return "", false
	}
}

func (l *TraceOrgLookup) cached(traceID string) (string, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.cache[traceID]
	if !ok || time.Now().After(e.expiresAt) {
		return "", false, false
	}
	return e.org, e.found, true
}

func (l *TraceOrgLookup) store(traceID, org string, found bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cache == nil {
		l.cache = make(map[string]orgCacheEntry)
	}
	if len(l.cache) >= traceOrgCacheMax {
		l.cache = make(map[string]orgCacheEntry, traceOrgCacheMax)
	}
	l.cache[traceID] = orgCacheEntry{
		org:       org,
		found:     found,
		expiresAt: time.Now().Add(traceOrgCacheTTL),
	}
}

// isNoRows recognises the driver's empty-result error. clickhouse-go returns
// sql.ErrNoRows from QueryRow on an empty result set; the string check is a
// belt-and-braces guard because a driver upgrade that wrapped it differently
// would otherwise turn "unknown trace" into "lookup failed" — same refusal
// either way, but it would stop being cached and add a query per webhook score.
func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "no rows")
}
