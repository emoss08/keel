package keel

import (
	"context"
	"errors"
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

// batch sends stmts in one round trip. last marks the runner's final batch: an implicit
// transaction commits in the same round trip.
func (r *runner) batch(ctx context.Context, stmts []*stmt, last bool) error {
	if len(stmts) == 0 {
		return nil
	}
	conn, err := r.acquire(ctx)
	if err != nil {
		return err
	}
	b := &pgx.Batch{}
	pre := 0
	beginning := r.tx != nil && !r.tx.begun
	if beginning {
		if pre, err = r.tx.queueBegin(b); err != nil {
			return err
		}
	}
	for _, s := range stmts {
		b.Queue(s.sql, s.args...)
	}
	commit := r.own && last
	if commit {
		b.Queue("COMMIT")
	}

	events := r.db.startEvents(ctx, stmts)
	start := time.Now()
	br := conn.SendBatch(ctx, b)
	runErr := r.readBatch(br, pre, stmts, commit)
	closeErr := br.Close()
	if runErr == nil {
		runErr = closeErr
	}
	r.db.endEvents(ctx, events, stmts, time.Since(start), runErr)

	if beginning {
		r.tx.begun = true
	}
	if commit && runErr == nil {
		r.tx.committed = true
	}
	return runErr
}

func (r *runner) readBatch(br pgx.BatchResults, pre int, stmts []*stmt, commit bool) error {
	for range pre {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	for _, s := range stmts {
		if s.scan == nil {
			tag, err := br.Exec()
			if err != nil {
				return err
			}
			s.tag = tag
			continue
		}
		rows, err := br.Query()
		if err != nil {
			return err
		}
		scanErr := s.scan(rows)
		rows.Close()
		if scanErr != nil {
			return scanErr
		}
		if err := rows.Err(); err != nil {
			return err
		}
		s.tag = rows.CommandTag()
	}
	if commit {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
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

var errClosedRows = errors.New("keel: rows closed")
