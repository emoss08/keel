package analytics

import (
	"context"
	"time"

	"github.com/emoss08/keel"
)

// TenantLock exposes the first advisory lock key of a tenant's slots to the tests.
var TenantLock = tenantLock

// SupersedeTag exposes the application_name of a keyed run to the tests.
var SupersedeTag = supersedeTag

// SleepThroughRunner runs pg_sleep(seconds) through the runner's guarded transaction with a
// timeout, so the tests see how a statement past its timeout is reported.
func SleepThroughRunner(ctx context.Context, r *Runner, comp *Compiled, seconds float64, timeout time.Duration) error {
	return r.execute(ctx, Request{}, comp, timeout, func(ctx context.Context, _ *Compiled) error {
		_, err := keel.Exec(ctx, r.db, "SELECT pg_sleep($1)", seconds)
		return err
	})
}
