package keel

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrNotFound is returned by One, First and Get when no row matches.
	ErrNotFound = errors.New("keel: no rows")
	// ErrNotSingular is returned by One when more than one row matches.
	ErrNotSingular = errors.New("keel: more than one row")
	// ErrConflict is matched by a *ConflictError: Save found a different version.
	ErrConflict = errors.New("keel: version conflict")
	// ErrNoScope is returned for a statement on a tenant table without a tenant or system scope.
	ErrNoScope = errors.New("keel: no tenant or system scope on the context")
	// ErrAllTenantsRequired is returned for a statement on a tenant table under a system scope
	// that did not declare AllTenants.
	ErrAllTenantsRequired = errors.New("keel: system scope statement on a tenant table must declare AllTenants")
	// ErrSystemScopeRequired is returned when AllTenants is used without a system scope.
	ErrSystemScopeRequired = errors.New("keel: AllTenants requires a system scope")
	// ErrTenantMismatch is returned when a row's tenant columns differ from the scope's.
	ErrTenantMismatch = errors.New("keel: row belongs to a different tenant than the scope")
	// ErrUnbounded is returned by List without a Limit.
	ErrUnbounded = errors.New("keel: List needs a Limit; use Page, or All for a set known to be small")
	// ErrReadOnly is returned by a write inside a read-only transaction.
	ErrReadOnly = errors.New("keel: write inside a read-only transaction")
	// ErrScopeMismatch is returned by a statement, Read, Write or Savepoint that would join the
	// transaction on the context under another scope than the one the transaction began with. Run
	// such work in its own transaction with Detached.
	ErrScopeMismatch = errors.New("keel: the context's scope differs from its transaction's; use keel.Detached for a transaction of its own")

	errRawArgs = errors.New("keel: Raw has fewer arguments than placeholders")
)

// ConflictError reports a Save whose version no longer matched the stored row.
type ConflictError struct {
	Table   string
	Version int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("keel: %s changed since version %d was read", e.Table, e.Version)
}

// Is makes errors.Is(err, ErrConflict) true.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

func pgError(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// Code returns the SQLSTATE of a Postgres error, or "".
func Code(err error) string {
	if pe := pgError(err); pe != nil {
		return pe.Code
	}
	return ""
}

// Constraint returns the constraint a Postgres error names, or "".
func Constraint(err error) string {
	if pe := pgError(err); pe != nil {
		return pe.ConstraintName
	}
	return ""
}

func isCode(err error, code string, constraints []string) bool {
	pe := pgError(err)
	if pe == nil || pe.Code != code {
		return false
	}
	if len(constraints) == 0 {
		return true
	}
	for _, c := range constraints {
		if pe.ConstraintName == c {
			return true
		}
	}
	return false
}

// IsUnique reports a unique violation, optionally on one of the named constraints.
func IsUnique(err error, constraints ...string) bool { return isCode(err, "23505", constraints) }

// IsForeignKey reports a foreign key violation, optionally on one of the named constraints.
func IsForeignKey(err error, constraints ...string) bool { return isCode(err, "23503", constraints) }

// IsCheck reports a check constraint violation, optionally on one of the named constraints.
func IsCheck(err error, constraints ...string) bool { return isCode(err, "23514", constraints) }

// IsExclusion reports an exclusion constraint violation.
func IsExclusion(err error, constraints ...string) bool { return isCode(err, "23P01", constraints) }

// IsNotNull reports a not-null violation.
func IsNotNull(err error) bool { return isCode(err, "23502", nil) }

// IsRetryable reports a serialization failure or deadlock, which a retried transaction can
// resolve.
func IsRetryable(err error) bool {
	code := Code(err)
	return code == "40001" || code == "40P01"
}
