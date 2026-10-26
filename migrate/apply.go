package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// execer runs a statement on a connection or in a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// run is one Up.
type run struct {
	m     *Migrator
	watch *watcher
	index int
	total int
}

// up takes the lock, plans and applies.
func (r *run) up(ctx context.Context, o UpOptions) (Result, error) {
	var res Result
	if err := r.m.ensureTable(ctx); err != nil {
		return res, err
	}
	unlock, err := r.m.lock(ctx)
	if err != nil {
		return res, err
	}
	defer unlock()
	recs, err := r.m.records(ctx)
	if err != nil {
		return res, err
	}
	if res.Plan, err = plan(r.m.migrations, recs, r.m.opts, o); err != nil {
		return res, err
	}
	r.total = res.Plan.Total()
	for _, mg := range res.Plan.Marks {
		if err := r.mark(ctx, mg); err != nil {
			return res, err
		}
		res.Marked = append(res.Marked, mg)
	}
	steps := append(append([]Migration(nil), res.Plan.Pending...), res.Plan.Repeatables...)
	for i, mg := range steps {
		var after []string
		if i == len(steps)-1 && !mg.Directives.NoTx {
			after = res.Plan.After
		}
		if err := r.apply(ctx, mg, after); err != nil {
			err = fmt.Errorf("migrate: %s: %w", mg.ID(), err)
			if len(res.Applied) > 0 && len(res.Plan.After) > 0 {
				err = errors.Join(err, r.afterTx(ctx, res.Plan.After))
			}
			return res, err
		}
		res.Applied = append(res.Applied, mg)
	}
	if len(steps) > 0 && steps[len(steps)-1].Directives.NoTx && len(res.Plan.After) > 0 {
		return res, r.afterTx(ctx, res.Plan.After)
	}
	return res, nil
}

// mark records a baseline as applied without running it, replacing the row of the migration with
// its version that the database applied before the squash.
func (r *run) mark(ctx context.Context, mg Migration) error {
	t := r.m.opts.table()
	err := pgx.BeginFunc(ctx, r.m.conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM "+t+" WHERE version = $1", mg.Version); err != nil {
			return err
		}
		return record(ctx, tx, r.m.opts, mg, 0)
	})
	if err != nil {
		return fmt.Errorf("migrate: mark baseline %s: %w", mg.ID(), err)
	}
	r.m.emit(Event{Kind: BaselineMarked, Migration: &mg, Index: r.index, Total: r.total})
	r.index++
	return nil
}

// apply runs one migration, with after in its transaction.
func (r *run) apply(ctx context.Context, mg Migration, after []string) error {
	start := time.Now()
	r.m.emit(Event{Kind: MigrationStart, Migration: &mg, Index: r.index, Total: r.total})
	var err error
	if mg.Directives.NoTx {
		err = r.applyNoTx(ctx, &mg, start)
	} else {
		err = r.retry(ctx, &mg, mg.Directives, 0, 0, func() ([]Blocker, error) {
			return r.attemptTx(ctx, mg, after, start)
		})
	}
	r.m.emit(Event{Kind: MigrationEnd, Migration: &mg, Index: r.index, Total: r.total, Duration: time.Since(start), Err: err})
	r.index++
	return err
}

// attemptTx runs a transactional migration once and returns the backends that blocked it.
func (r *run) attemptTx(ctx context.Context, mg Migration, after []string, start time.Time) ([]Blocker, error) {
	tx, err := r.m.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lt := r.m.opts.lockTimeout(mg.Directives)
	if err := setTimeouts(ctx, tx, lt, mg.Directives.StatementTimeout, true); err != nil {
		return nil, err
	}
	stop := r.watchFor(ctx, lt)
	err = r.body(ctx, tx, mg)
	if err == nil {
		err = record(ctx, tx, r.m.opts, mg, time.Since(start))
	}
	if err == nil {
		err = r.runAfter(ctx, tx, after)
	}
	if blockers := stop(); err != nil {
		return blockers, err
	}
	return nil, tx.Commit(ctx)
}

// body runs a transactional migration's SQL or Go function.
func (r *run) body(ctx context.Context, tx pgx.Tx, mg Migration) error {
	if mg.Kind == KindGo {
		return mg.up(ctx, tx)
	}
	if strings.TrimSpace(mg.SQL) == "" {
		return nil
	}
	_, err := tx.Exec(ctx, mg.SQL)
	return err
}

// applyNoTx runs a notx migration statement by statement, each with its own retries, under
// session-level timeouts it resets afterwards, then records it.
func (r *run) applyNoTx(ctx context.Context, mg *Migration, start time.Time) error {
	conn := r.m.conn
	lt := r.m.opts.lockTimeout(mg.Directives)
	if err := setTimeouts(ctx, conn, lt, mg.Directives.StatementTimeout, false); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "RESET lock_timeout; RESET statement_timeout") }()
	steps := r.noTxSteps(*mg)
	for i, step := range steps {
		r.m.emit(Event{Kind: Statement, Migration: mg, Index: r.index, Total: r.total, Statement: i + 1,
			Statements: len(steps), SQL: step.sql})
		err := r.retry(ctx, mg, mg.Directives, i+1, len(steps), func() ([]Blocker, error) {
			stop := r.watchFor(ctx, lt)
			err := step.run(ctx, conn)
			return stop(), err
		})
		if err != nil {
			if len(steps) > 1 {
				return fmt.Errorf("statement %d of %d: %w", i+1, len(steps), err)
			}
			return err
		}
	}
	return record(ctx, conn, r.m.opts, *mg, time.Since(start))
}

// noTxStep is one statement of a notx migration.
type noTxStep struct {
	sql string
	run func(context.Context, *pgx.Conn) error
}

// noTxSteps returns the statements of a notx migration: its split SQL, or its Go function.
func (r *run) noTxSteps(mg Migration) []noTxStep {
	if mg.Kind == KindGo {
		return []noTxStep{{run: mg.upConn}}
	}
	var out []noTxStep
	for _, s := range mg.Statements() {
		out = append(out, noTxStep{sql: s, run: func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, s)
			return err
		}})
	}
	return out
}

// afterTx runs the After statements in a transaction of their own.
func (r *run) afterTx(ctx context.Context, after []string) error {
	lt := r.m.opts.lockTimeout(Directives{})
	err := r.retry(ctx, nil, Directives{}, 0, 0, func() ([]Blocker, error) {
		tx, err := r.m.conn.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if err := setTimeouts(ctx, tx, lt, 0, true); err != nil {
			return nil, err
		}
		stop := r.watchFor(ctx, lt)
		err = r.runAfter(ctx, tx, after)
		if blockers := stop(); err != nil {
			return blockers, err
		}
		return nil, tx.Commit(ctx)
	})
	if err != nil {
		return fmt.Errorf("migrate: after: %w", err)
	}
	return nil
}

// runAfter runs the After statements in tx.
func (r *run) runAfter(ctx context.Context, tx pgx.Tx, after []string) error {
	for _, s := range after {
		r.m.emit(Event{Kind: After, Index: r.index, Total: r.total, SQL: s})
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("after %q: %w", s, err)
		}
	}
	return nil
}

// retry runs attempt, retrying it with backoff while it fails with a lock timeout and retries
// remain, and sends Retry before each retry.
func (r *run) retry(ctx context.Context, mg *Migration, d Directives, stmt, stmts int,
	attempt func() ([]Blocker, error),
) error {
	n := r.m.opts.retries(d)
	b := r.m.opts.backoff()
	for a := 1; ; a++ {
		blockers, err := attempt()
		if err == nil || !isLockTimeout(err) {
			return err
		}
		if a > n {
			if len(blockers) > 0 {
				err = fmt.Errorf("%w; blocked by %s", err, describeBlockers(blockers))
			}
			if n > 0 {
				err = fmt.Errorf("after %d attempts: %w", a, err)
			}
			return err
		}
		r.m.emit(Event{Kind: Retry, Migration: mg, Index: r.index, Total: r.total, Statement: stmt,
			Statements: stmts, Attempt: a + 1, Blockers: blockers, Err: err})
		if err := sleep(ctx, b.NextBackOff()); err != nil {
			return err
		}
	}
}

// watchFor starts probing for blockers when statements wait on locks under a lock timeout.
func (r *run) watchFor(ctx context.Context, lockTimeout time.Duration) func() []Blocker {
	if lockTimeout <= 0 {
		return func() []Blocker { return nil }
	}
	return r.watch.watch(ctx, probeInterval(lockTimeout))
}

// setTimeouts sets lock_timeout and statement_timeout for the transaction, or the session when
// local is false; zero disables a timeout.
func setTimeouts(ctx context.Context, ex execer, lock, stmt time.Duration, local bool) error {
	_, err := ex.Exec(ctx, "SELECT set_config('lock_timeout', $1, $3), set_config('statement_timeout', $2, $3)",
		millis(lock), millis(stmt), local)
	return err
}

// millis formats d as a Postgres duration in milliseconds; zero or less is "0".
func millis(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return fmt.Sprintf("%dms", max(d.Milliseconds(), 1))
}

// record writes or replaces a migration's bookkeeping row.
func record(ctx context.Context, ex execer, o Options, mg Migration, d time.Duration) error {
	_, err := ex.Exec(ctx, `INSERT INTO `+o.table()+` (version, name, source, kind, checksum, duration_ms)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (version, name) DO UPDATE SET source = EXCLUDED.source, kind = EXCLUDED.kind,
	checksum = EXCLUDED.checksum, applied_at = now(), duration_ms = EXCLUDED.duration_ms`,
		mg.Version, mg.Name, mg.Source, mg.Kind.String(), mg.Checksum, d.Milliseconds())
	return err
}
