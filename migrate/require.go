package migrate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrBehind is returned by Require when the database has not applied the version a server needs.
var ErrBehind = errors.New("migrate: the database is behind")

// BehindError is the error Require returns; it wraps ErrBehind.
type BehindError struct {
	// Want is the version the caller needs.
	Want string
	// Have is the newest version the database applied, or "" when it applied none.
	Have string
}

// Error describes the gap.
func (e *BehindError) Error() string {
	if e.Have == "" {
		return fmt.Sprintf("migrate: the database has applied no migration; it needs %s", e.Want)
	}
	return fmt.Sprintf("migrate: the database is at %s and needs %s", e.Have, e.Want)
}

// Unwrap returns ErrBehind.
func (e *BehindError) Unwrap() error { return ErrBehind }

// Querier runs a query returning one row; *pgx.Conn, *pgxpool.Pool and pgx.Tx satisfy it.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// requireSQL reports whether version $1 is applied, directly or through a newer baseline, and the
// newest applied version.
const requireSQL = `SELECT EXISTS (SELECT 1 FROM ` + Table + ` WHERE version = $1)
	OR EXISTS (SELECT 1 FROM ` + Table + ` WHERE kind = 'baseline' AND version >= $1),
	coalesce((SELECT max(version) FROM ` + Table + ` WHERE version <> ''), '')`

// Require returns an error wrapping ErrBehind, a *BehindError, when the database has not applied
// version, which is usually the generated schema.Version a server was built with. A database
// ahead of version passes, which is what a rolling deploy needs. It reads keel_migrations through
// the search path, so a table in another schema needs that schema on the search path; a missing
// table is behind. An empty version passes.
func Require(ctx context.Context, q Querier, version string) error {
	if version == "" {
		return nil
	}
	var ok bool
	var have string
	err := q.QueryRow(ctx, requireSQL, version).Scan(&ok, &have)
	switch {
	case isUndefinedTable(err):
		return &BehindError{Want: version}
	case err != nil:
		return fmt.Errorf("migrate: require %s: %w", version, err)
	case !ok:
		return &BehindError{Want: version, Have: have}
	}
	return nil
}

// RequireWait polls Require every interval until the database has applied version or ctx ends,
// for a server that starts alongside its migration job. Errors other than ErrBehind, such as a
// database not accepting connections yet, are retried too; when ctx ends, the returned error joins
// ctx's error and the last one.
func RequireWait(ctx context.Context, q Querier, version string, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	var last error
	for {
		err := Require(ctx, q, version)
		if err == nil {
			return nil
		}
		if last == nil || ctx.Err() == nil {
			last = err
		}
		if serr := sleep(ctx, interval); serr != nil {
			return errors.Join(serr, last)
		}
	}
}
