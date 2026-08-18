// Package postgreserr maps a Postgres pq error into a public apierr.Code.
//
// The mapping is the boundary where raw database diagnostics stop existing:
// the public body never carries the SQLSTATE, the column, the table, or any
// other string Postgres would put in its message. The complete error is
// logged by the caller alongside the rendered response, correlated by request
// id.
//
// # Why a separate package
//
// the pkgerrors/citext/pgconn surface in this codebase is imported through
// jackc/pgx, and the canonical SQLSTATE is wrapped three layers deep. A small
// package with a single function keeps the public surface small AND makes it
// possible to write one test that asserts every Console handlers' classification
// matches expectations, instead of `pq: X (SQLSTATE Y)` strings on every error
// path.
package postgreserr

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ffxnexus/nexus/internal/apierr"
)

// Classify walks the error chain and returns the apierr.Code that best matches
// the underlying Postgres condition. It returns CodeInternalError when the
// error is not a Postgres error (e.g. a fmt error, a context deadline, or a
// network error with no SQLSTATE).
//
// The mapping is deliberately narrow. Every match is a class where Postgres's
// message is known to contain a protected substring (table, column, constraint)
// and where the remediating behaviour is identical for all rows of the class.
func Classify(err error) apierr.Code {
	if err == nil {
		return ""
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		// Context cancellation is a dependency_unavailable to the caller, not an
		// internal error: it means the upstream owner (the database) took too
		// long, which is operationally distinct from "we crashed".
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return apierr.CodeDependencyUnavailable
		}
		return apierr.CodeInternalError
	}
	switch pg.Code {
	// Integrity violations are caused by client input. The pg.Message may carry
	// table/column names, so the caller MUST not place pg.Message in the body.
	case "23505": // unique_violation
		return apierr.CodeConflict
	case "23503": // foreign_key_violation
		return apierr.CodeConflict
	case "23514": // check_violation
		return apierr.CodeInvalidRequest
	case "22P02": // invalid_text_representation
		return apierr.CodeInvalidRequest
	case "22023": // invalid_parameter_value
		return apierr.CodeInvalidRequest

	// Data not found at the row level — but the *resource* may exist under a
	// different org. Distinguished at the handler level so cross-tenant reads
	// answer 404 not 403.
	case "42703": // undefined_column — schema drift. Surfaces as a fault.
		return apierr.CodeInternalError
	case "42P01": // undefined_table — schema drift. Surfaces as a fault.
		return apierr.CodeInternalError
	case "42P18": // indeterminate_datatype — query bug, not user input
		return apierr.CodeInternalError

	// Authorization-style failures at the protocol level — the cluster's
	// permissions rejected the request. Surfaces as forbidden so an operator
	// notices; never reveals the role that backed the rejection.
	case "42501": // insufficient_privilege
		return apierr.CodeForbidden
	}
	return apierr.CodeInternalError
}

// Message returns the short, public-safe human message for an apierr.Code
// derived from a Postgres error. The complete pg error is appended to the
// server log by the caller; it MUST NOT reach the body.
//
// The function returns only an internal-looking message, with no Postgres
// keyword, no SQLSTATE, no identifier.
func Message(code apierr.Code) string {
	switch code {
	case apierr.CodeConflict:
		return "the resource already exists or conflicts with another"
	case apierr.CodeInvalidRequest:
		return "the input failed validation"
	case apierr.CodeNotFound:
		return "the resource was not found"
	case apierr.CodeForbidden:
		return "access is forbidden for the caller"
	case apierr.CodeDependencyUnavailable:
		return "the database is not currently available"
	}
	return ""
}
