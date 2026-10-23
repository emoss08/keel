package keel

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stmt is one statement of a batch.
type stmt struct {
	sql   string
	args  []any
	op    string
	table string

	scan func(rows pgx.Rows) error
	tag  pgconn.CommandTag

	customPlan bool
}

// runner executes the statements of one terminal call. It joins the transaction on the context,
// opens an implicit one when Config.OnBegin is set, or runs on a single pooled connection.
type runner struct {
	db   *DB
	sc   *scopeState
	tx   *txState
	own  bool
	conn *pgxpool.Conn
}

func (db *DB) newRunner(ctx context.Context, write bool) (*runner, error) {
	r := &runner{db: db, sc: db.resolveScope(ctx)}
	if tx := txFrom(ctx, db); tx != nil {
		if write && tx.readOnly {
			return nil, ErrReadOnly
		}
		r.tx = tx
		return r, nil
	}
	if db.cfg.OnBegin != nil {
		r.tx = &txState{db: db, readOnly: !write, implicit: true, scopeCtx: ctx}
		r.own = true
	}
	return r, nil
}

func (r *runner) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if r.tx != nil {
		return r.tx.acquire(ctx)
	}
	if r.conn != nil {
		return r.conn, nil
	}
	conn, err := r.db.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	r.conn = conn
	return conn, nil
}

// started returns the runner's connection with its transaction, if it has one, begun, for a
// statement that cannot join a batch, such as COPY.
func (r *runner) started(ctx context.Context) (*pgxpool.Conn, error) {
	if r.tx != nil {
		if err := r.tx.begin(ctx); err != nil {
			return nil, err
		}
	}
	return r.acquire(ctx)
}

// batch sends stmts in one round trip. last marks the runner's final batch: an implicit
// transaction commits in the same round trip.
func (r *runner) batch(ctx context.Context, stmts []*stmt, last bool) error {
	if len(stmts) == 0 {
		return nil
	}
	custom := false
	if r.db.cfg.PlanCache != PlanCacheOff {
		for _, s := range stmts {
			custom = custom || s.customPlan
		}
	}
	if custom && r.tx == nil {
		r.tx = &txState{db: r.db, readOnly: true, implicit: true, conn: r.conn}
		r.conn = nil
		r.own = true
	}
	conn, err := r.acquire(ctx)
	if err != nil {
		return err
	}
	beginning := r.tx != nil && !r.tx.begun
	commit := r.own && last
	if len(stmts) == 1 && !beginning && !commit && !custom {
		return r.direct(ctx, conn, stmts[0])
	}
	b := &pgx.Batch{}
	pre := 0
	if beginning {
		if pre, err = r.tx.queueBegin(b); err != nil {
			return err
		}
	}
	for _, s := range stmts {
		if s.customPlan && custom {
			b.Queue("SELECT set_config('plan_cache_mode', 'force_custom_plan', true)")
		}
		b.Queue(s.sql, s.args...)
		if s.customPlan && custom {
			b.Queue("SELECT set_config('plan_cache_mode', $1, true)", r.db.planMode())
		}
	}
	if commit {
		b.Queue("COMMIT")
	}

	events, qctx := r.db.startEvents(ctx, stmts)
	start := time.Now()
	br := conn.SendBatch(qctx, b)
	runErr := r.readBatch(br, pre, stmts, commit, custom)
	closeErr := br.Close()
	if runErr == nil {
		runErr = closeErr
	}
	r.db.endEvents(events, stmts, time.Since(start), runErr)

	if beginning {
		r.tx.begun = true
	}
	if commit && runErr == nil {
		r.tx.committed = true
	}
	return runErr
}

func (r *runner) readBatch(br pgx.BatchResults, pre int, stmts []*stmt, commit, custom bool) error {
	for range pre {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	for _, s := range stmts {
		wrap := s.customPlan && custom
		if wrap {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
		if err := readOne(br, s); err != nil {
			return err
		}
		if wrap {
			if _, err := br.Exec(); err != nil {
				return err
			}
		}
	}
	if commit {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

func readOne(br pgx.BatchResults, s *stmt) error {
	if s.scan == nil {
		tag, err := br.Exec()
		if err != nil {
			return err
		}
		s.tag = tag
		return nil
	}
	rows, err := br.Query()
	if err != nil {
		return err
	}
	return scanRows(rows, s)
}

// scanRows runs a statement's scanner over rows. A server error surfaces on the rows, so it wins
// over the scanner's error, which it may have caused.
func scanRows(rows pgx.Rows, s *stmt) error {
	scanErr := s.scan(rows)
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	s.tag = rows.CommandTag()
	return nil
}

// direct sends one statement outside a pipeline. pgx's pipeline keeps per-message bookkeeping a
// lone statement does not need.
func (r *runner) direct(ctx context.Context, conn *pgxpool.Conn, s *stmt) error {
	stmts := []*stmt{s}
	events, qctx := r.db.startEvents(ctx, stmts)
	start := time.Now()
	var err error
	if s.scan == nil {
		s.tag, err = conn.Exec(qctx, s.sql, s.args...)
	} else {
		var rows pgx.Rows
		rows, err = conn.Query(qctx, s.sql, s.args...)
		if err == nil {
			err = scanRows(rows, s)
		}
	}
	r.db.endEvents(events, stmts, time.Since(start), err)
	return err
}

// one sends a single statement.
func (r *runner) one(ctx context.Context, s *stmt, last bool) error {
	return r.batch(ctx, []*stmt{s}, last)
}

// close ends an implicit transaction (committing when the last batch did not) and releases the
// connection. It returns err, joined with any error from ending the transaction.
func (r *runner) close(ctx context.Context, err error) error {
	if r.conn != nil {
		r.conn.Release()
		r.conn = nil
	}
	if !r.own {
		return err
	}
	err = r.tx.end(ctx, err)
	if err == nil {
		r.tx.runAfterCommit(ctx)
	}
	return err
}
