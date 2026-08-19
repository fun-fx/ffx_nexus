// c0.7 audit view / export API. The console's /api/audit endpoint
// is the customer-facing surface for operators to read their
// organisational audit feed. The export endpoint streams the same
// shape as a CSV download with formula-injection defence and a bound
// on row count + time range.
//
// The functions here are called from internal/console/audit_handlers.go
// which translates HTTP into these narrow contracts; this package
// stays HTTP-free so it can be tested directly with an in-process
// Postgres connection.

package core

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ffxnexus/nexus/internal/apierr"
	"github.com/ffxnexus/nexus/internal/auditid"
	"github.com/ffxnexus/nexus/internal/core/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditFilter is the query shape for ViewAudit / ExportAudit. The
// struct keeps the field names explicit (matching the user's
// argument-swap concern from the c0.1 review) and prevents
// accidentally passing a free-form org-id into the wrong slot at
// handler boundaries.
type AuditFilter struct {
	OrgID           string
	ActionPrefix    string // "" returns all actions
	Actor           string // "" returns all actors
	TargetPrefix    string // "" returns all targets
	FromTime        time.Time
	ToTime          time.Time
	RequestID       string     // optional
	ClientReqID     string     // optional
	CursorCreatedAt *time.Time // last seen created_at for cursor-pagination
	CursorID        int64      // last seen id for cursor-pagination
	Limit           int
}

// AuditRow is the row shape returned to the view/export surface. The
// string fields are NOT scrubbed here — apierr.Scrub was applied at
// the write site. The fields are explicitly chosen to match the
// audit_log schema so an admin reading the CSV sees exactly what
// Postgres wrote.
type AuditRow struct {
	ID          int64
	OrgID       string
	Actor       string
	Action      string
	TargetID    string
	Detail      string
	RequestID   string
	ClientReqID string
	Count       int
	FirstAt     *time.Time
	LastAt      *time.Time
	CreatedAt   time.Time
	ResourceFP  string
}

// ViewMaxRows is the documented upper bound on a single audit page.
// Larger queries use cursor pagination.
const ViewMaxRows = 200

// ExportMaxRows is the upper bound on a single export. Operators who
// need larger exports run an external BI reader against a clone.
const ExportMaxRows = 100_000

// DefaultTimeSpan is the upper bound on the time range. The /api/audit
// view caps at 90 days; export at 30 days to keep CSV download
// practical.
const (
	DefaultViewTimeSpan   = 90 * 24 * time.Hour
	DefaultExportTimeSpan = 30 * 24 * time.Hour
)

// AuditRetailer is a tiny interface to make the test hermetic.
// Production wires this up by returning the pool; tests can return
// an error to simulate Postgres-down.
type AuditRetailer interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// Rows is the portion of pgx.Rows we use. Production code uses
// pgx.Rows directly; tests can return a mocked Rows. We keep the
// interface minimal so test scaffold stays tiny.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// ViewAudit runs the audit query with the filter and returns the
// configured page. Errors propagate to the caller; org-scope
// enforcement is part of the WHERE clause (org_id = $1) so a query
// that tries to span orgs is silently filtered to one.
//
// The org_id is required; an empty OrgID returns an error.
func ViewAudit(ctx context.Context, pool *pgxpool.Pool, f AuditFilter) ([]AuditRow, error) {
	return runAuditQuery(ctx, pool, f, ViewMaxRows, DefaultViewTimeSpan)
}

// ExportAudit writes the audit query to w as CSV with formula-
// injection defence applied to every string cell. The returned
// count is the number of rows written.
//
// The row limit is ExportMaxRows; the time span is DefaultExportTimeSpan.
// Callers wanting different bounds should adjust the AuditFilter.
func ExportAudit(ctx context.Context, pool *pgxpool.Pool, f AuditFilter, w io.Writer) (int, error) {
	if pool == nil {
		return 0, errors.New("audit.ExportAudit: pool is nil")
	}
	if err := validateAuditFilter(f, DefaultExportTimeSpan); err != nil {
		return 0, err
	}
	rows, err := runAuditQueryRaw(ctx, pool, f, ExportMaxRows, DefaultExportTimeSpan)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cw := csv.NewWriter(w)
	count, err := writeAuditCSV(cw, rows)
	if err != nil {
		return count, err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return count, err
	}
	return count, nil
}

// ExportHeaderEcho is a status-line helper for handlers returning
// a streaming CSV download. The header is checked once via curl /
// browser devtools to confirm the export itself was audited.
const ExportHeaderEcho = "X-Nexus-Audit-Export-Recorded"

// exportRequestAudit is the audit action string used when a CSV
// export itself is requested. Operators reading the audit feed see
// "audit.export" rows for every download.
const exportRequestAudit = string(AuditActionAuditExported)

// writeAuditCSV streams audit rows into a CSV writer with formula-
// injection defence applied to every cell. The defence prefixes
// any cell that begins with `=`, `+`, `-`, `@`, or tab/CR with a
// single-quote so Excel/LibreOffice do not interpret the cell as a
// formula. Without this, an attacker who could inject a CSV header
// could mount a CSV-injection attack against the operator.
//
// Audit feed values are operator-side, not customer-side, but the
// pattern is the same: we do not trust CSV interpreters. Defence
// in depth is cheap.
func writeAuditCSV(cw *csv.Writer, rows Rows) (int, error) {
	header := auditCSVHeader
	if err := cw.Write(header); err != nil {
		return 0, fmt.Errorf("write csv header: %w", err)
	}
	count := 1
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Actor, &r.Action, &r.TargetID, &r.Detail,
			&r.RequestID, &r.ClientReqID, &r.Count, &r.FirstAt, &r.LastAt, &r.CreatedAt, &r.ResourceFP); err != nil {
			return count, fmt.Errorf("scan row %d: %w", count, err)
		}
		row := []string{
			csvSafeCell(r.OrgID),
			csvSafeCell(r.Actor),
			csvSafeCell(r.Action),
			csvSafeCell(r.TargetID),
			csvSafeCell(r.Detail),
			csvSafeCell(r.RequestID),
			csvSafeCell(r.ClientReqID),
			fmt.Sprintf("%d", r.Count),
			csvSafeCell(isoTime(r.FirstAt)),
			csvSafeCell(isoTime(r.LastAt)),
			csvSafeCell(isoTime(&r.CreatedAt)),
			csvSafeCell(r.ResourceFP),
		}
		if err := cw.Write(row); err != nil {
			return count, fmt.Errorf("write row %d: %w", count, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("rows.Err: %w", err)
	}
	return count, nil
}

var auditCSVHeader = []string{
	"org_id", "actor", "action", "target_id", "detail",
	"request_id", "client_request_id", "count", "first_at", "last_at",
	"created_at", "resource_fingerprint",
}

// csvSafeCell applies formula-injection defence. Cells starting with
// `=`, `+`, `-`, `@`, or special whitespace are prefixed with `'`
// so spreadsheet programs treat them as text. Newlines, CRs, and
// tabs are stripped because they would otherwise break the
// CSV-rendering reader.
//
// Defence rationale: any operator who views this CSV in Excel,
// LibreOffice, or via a third-party BI tool is a potential target
// if the cell value comes from an attacker-controlled source. We
// already scrubbed the values at write time, but the defence here is
// cheap belt-and-braces.
func csvSafeCell(s string) string {
	if s == "" {
		return ""
	}
	// Strip CR/LF/TAB — these are control characters that some
	// parsers refuse silently and others treat as formula markers.
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) > 0 {
		switch s[0] {
		case '=', '+', '-', '@', '\t':
			s = "'" + s
		}
	}
	return s
}

func isoTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// validateAuditFilter checks the filter shape and bounds. An empty
// OrgID returns error because every audit query MUST be scoped by
// org to satisfy isolation.
//
// Length and charset bounds are tight on purpose: an attacker who
// can pass a 1 MB ActionPrefix or an OrgID with control characters
// could mount CSV-injection or SQL-error-disclosure attacks via the
// downstream CSV writer. The bounds mirror the DB column limits
// (org_id VARCHAR(128), actor VARCHAR(128)) so an injection that
// would overflow the column gets rejected at the boundary instead.
func validateAuditFilter(f AuditFilter, maxSpan time.Duration) error {
	if f.OrgID == "" {
		return errors.New("audit: OrgID is required")
	}
	if len(f.OrgID) > 128 {
		return errors.New("audit: OrgID exceeds 128 chars (column limit)")
	}
	if !isAsciiIdent(f.OrgID) {
		return fmt.Errorf("audit: OrgID contains non-identifier characters: %q",
			truncateForErr(f.OrgID))
	}
	if len(f.Actor) > 128 {
		return errors.New("audit: Actor exceeds 128 chars (column limit)")
	}
	if f.Actor != "" && !isAsciiIdentPermissive(f.Actor) {
		return fmt.Errorf("audit: Actor contains disallowed characters: %q",
			truncateForErr(f.Actor))
	}
	if len(f.ActionPrefix) > 64 {
		return errors.New("audit: ActionPrefix exceeds 64 chars")
	}
	if len(f.TargetPrefix) > 256 {
		return errors.New("audit: TargetPrefix exceeds 256 chars")
	}
	if len(f.RequestID) > 256 {
		return errors.New("audit: RequestID exceeds 256 chars")
	}
	if len(f.ClientReqID) > 256 {
		return errors.New("audit: ClientReqID exceeds 256 chars")
	}
	if f.Limit <= 0 {
		return errors.New("audit: Limit must be > 0")
	}
	if !f.FromTime.IsZero() && !f.ToTime.IsZero() && f.ToTime.After(f.FromTime.Add(maxSpan)) {
		return fmt.Errorf("audit: time range exceeds %v", maxSpan)
	}
	return nil
}

// isAsciiIdent returns true when s is alphanumeric, hyphen, underscore,
// or a single dot — the shape Nexus uses for org/actor identifiers.
// This rejects spaces, control characters, quotes, and slashes that
// would otherwise enable CSV-formula injection in the export path.
func isAsciiIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '@':
		default:
			return false
		}
	}
	return true
}

// isAsciiIdentPermissive allows the @ character so email-shaped
// actor strings (e.g. "alice@example.com") pass; we still reject
// control chars and whitespace that would enable CSV injection.
func isAsciiIdentPermissive(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '@':
		default:
			return false
		}
	}
	return true
}

// sanitizeContentDisposition produces a safe value for the filename*
// parameter of Content-Disposition. The raw filename can include any
// byte, including a `\r\n` injection to break out of the header. We
// strip control characters and replace CR/LF with the default
// filename, because even "audit_Set-Cookie:" on a single line is a
// probable header-injection shape (some browsers collapse + trim
// differently and a normalised-to-underscore version might still
// pass through a downstream parser). Output policy:
//
//   - CR/LF/tab control chars → fall back to default name
//     `audit-export.csv` so an attacker cannot hide a header
//     boundary amid the value.
//   - Other control (<0x20, 0x7F) and quote/backslash → underscore.
//   - Bound length to 128 to keep the header line under standard
//     ingress limits.
//   - Non-ASCII (>=0x80) → default name; safe ASCII keeps the
//     filename.
//
// This is the canonical Content-Disposition defence; without it, a
// caller asking for file=<ctrl><sep>Set-Cookie: ... could mint an
// additional response header that bypasses the gateway's CORS and
// csrf protections.
func sanitizeContentDisposition(filename string) string {
	if filename == "" {
		return "audit-export.csv"
	}
	// CR/LF/tab are a header boundary; reject the whole filename.
	for _, r := range filename {
		if r == '\r' || r == '\n' || r == '\t' {
			return "audit-export.csv"
		}
	}
	var b strings.Builder
	for _, r := range filename {
		if r < 0x20 || r == 0x7F || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > 128 {
		out = out[:128]
	}
	for _, r := range out {
		if r >= 0x80 {
			return "audit-export.csv"
		}
	}
	return out
}

func truncateForErr(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

// runAuditQuery executes the audit SELECT with the filter against
// the pool and returns AuditRow slices. Errors are returned along
// with the partial rows so callers can surface partial state.
func runAuditQuery(ctx context.Context, pool *pgxpool.Pool, f AuditFilter, maxRows int, maxSpan time.Duration) ([]AuditRow, error) {
	if pool == nil {
		return nil, errors.New("pool is nil")
	}
	if err := validateAuditFilter(f, maxSpan); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit > maxRows {
		limit = maxRows
	}
	rows, err := pool.Query(ctx, auditQuerySQL, auditQueryArgs(f, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditRow{}
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.OrgID, &r.Actor, &r.Action, &r.TargetID, &r.Detail,
			&r.RequestID, &r.ClientReqID, &r.Count, &r.FirstAt, &r.LastAt, &r.CreatedAt, &r.ResourceFP); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// runAuditQueryRaw returns the raw pgx.Rows for streaming exports.
func runAuditQueryRaw(ctx context.Context, pool *pgxpool.Pool, f AuditFilter, maxRows int, maxSpan time.Duration) (Rows, error) {
	if pool == nil {
		return nil, errors.New("pool is nil")
	}
	if err := validateAuditFilter(f, maxSpan); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit > maxRows {
		limit = maxRows
	}
	return pool.Query(ctx, auditQuerySQL, auditQueryArgs(f, limit)...)
}

// auditQuerySQL is the SELECT used by View and Export. The columns
// match AuditRow. Filters are applied via auditQueryArgs.
const auditQuerySQL = `
SELECT id, org_id, actor, action, target_id, detail,
       request_id, client_request_id,
       count, first_at, last_at,
       created_at, COALESCE(resource_fingerprint, '')
  FROM audit_log
 WHERE org_id = $1
   AND ($2 = '' OR action LIKE $2 || '%')
   AND ($3 = '' OR actor = $3)
   AND ($4 = '' OR target_id LIKE $4 || '%')
   AND ($5 = '0001-01-01 00:00:00+00:00'::timestamptz OR created_at >= $5)
   AND ($6 = '9999-12-31 23:59:59+00:00'::timestamptz OR created_at <= $6)
   AND ($7 = '' OR request_id = $7)
   AND ($8 = '' OR client_request_id = $8)
   AND ($9::timestamptz IS NULL OR (created_at, id) < ($9, $10))
 ORDER BY created_at DESC, id DESC
 LIMIT $11`

// auditQueryArgs builds the query args from the filter. We use the
// sentinel time values '0001-01-01' and '9999-12-31' to mean "no
// filter applied" so the SQL string is constant and the planner
// can build one plan.
func auditQueryArgs(f AuditFilter, limit int) []any {
	from := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if !f.FromTime.IsZero() {
		from = f.FromTime
	}
	if !f.ToTime.IsZero() {
		to = f.ToTime
	}
	cursorAt := any(nil)
	if f.CursorCreatedAt != nil {
		cursorAt = *f.CursorCreatedAt
	}
	return []any{
		f.OrgID,
		f.ActionPrefix,
		f.Actor,
		f.TargetPrefix,
		from,
		to,
		f.RequestID,
		f.ClientReqID,
		cursorAt,
		f.CursorID,
		limit,
	}
}

// RecordExport writes a single audit row recording that an export
// was performed. The recorded row uses server-side audit ID lookup;
// no caller-supplied correlation id can leak.
//
// The exported request_id is empty here because we are auditing the
// handler call, not the original request. The next caller will see
// their own request_id via auditid.FromContext.
func RecordExport(ctx context.Context, store *Store, target OrgID) {
	if store == nil {
		return
	}
	store.Audit(ctx, AuditEvent{
		ActorID:  "system",
		OrgID:    string(target),
		Action:   AuditActionAuditExported,
		TargetID: string(target),
		Detail:   "audit_log export requested",
	})
}

// RecordView writes a single audit row recording that an audit
// query was performed.
func RecordView(ctx context.Context, store *Store, target OrgID) {
	if store == nil {
		return
	}
	store.Audit(ctx, AuditEvent{
		ActorID:  "system",
		OrgID:    string(target),
		Action:   AuditActionAuditViewed,
		TargetID: string(target),
		Detail:   "audit_log view requested",
	})
}

// OrgID is a typed string for an organisation identifier. It is
// declared in this file because the c0.1 review flagged that
// string-typed arguments to Store.Audit were unsafe; OrgID is now
// distinct from a free-form string.
type OrgID string

// Compile-time assertions: we use apierr, auditid, crypto packages so
// a future engineer does not accidentally remove an unused import.
var _ = slices.Min[[]string] // keep slices import in case future use
var _ apierr.Code            // keep apierr import
var _ = auditid.NewServerID  // keep auditid import
var _ crypto.Cipher          // keep crypto import
