package keel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Exec runs one SQL statement that keel does not render, such as DDL or a write to a bookkeeping
// table without a model, and returns the number of rows it affected. It joins the transaction on
// the context, so the statement commits or rolls back with the rest of it; outside a transaction it
// runs as any terminal does, in an implicit transaction when Config.OnBegin is set. Inside a Read,
// Postgres refuses a statement that writes.
//
// Observers see the statement with the Op "exec". keel cannot see what the statement touches, so it
// bypasses scope checks, interceptors and hooks, always runs on the primary, records no commit
// position for OnReplica outside a Write, and changes no data version: call MarkChanged with the
// tables it writes. Bind values as $1, $2, … arguments; never format them into sql.
func Exec(ctx context.Context, db *DB, sql string, args ...any) (int64, error) {
	s := &stmt{sql: sql, args: args, op: "exec"}
	if err := runRaw(ctx, db, []*stmt{s}); err != nil {
		return 0, err
	}
	return s.tag.RowsAffected(), nil
}

// ExecBatch runs statements keel does not render in one round trip, in order, as Exec runs one:
// joining the transaction on the context, else in an implicit transaction when Config.OnBegin is
// set. It returns the rows each affected. The first failing statement aborts the round trip;
// inside a transaction the transaction is then failed, and outside one nothing commits.
// Observers see each statement with the Op "exec".
func ExecBatch(ctx context.Context, db *DB, stmts ...Statement) ([]int64, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	ss := make([]*stmt, len(stmts))
	for i, st := range stmts {
		ss[i] = &stmt{sql: st.SQL, args: st.Args, op: "exec"}
	}
	if err := runRaw(ctx, db, ss); err != nil {
		return nil, err
	}
	out := make([]int64, len(ss))
	for i, s := range ss {
		out[i] = s.tag.RowsAffected()
	}
	return out, nil
}

// QueryRows runs one SQL statement keel does not render and calls fn for each row it returns, in
// order, with the rows positioned on that row: read it with Scan, Values or a pgx row function
// such as pgx.RowToStructByName. It joins the transaction on the context, Read or Write, so a read inside a
// Read with ReplicaTx runs on that replica; outside a transaction it runs on the primary, in an
// implicit transaction when Config.OnBegin is set. An error from fn stops the rows and is
// returned. fn must not run statements of its own on the same transaction: the connection is
// busy until the rows are read.
//
// Observers see the statement with the Op "query". As with Exec, keel cannot see what the
// statement touches: scope checks, interceptors, read filters and hooks do not apply, and a
// write through it changes no data version. Bind values as $1, $2, … arguments.
func QueryRows(ctx context.Context, db *DB, fn func(row pgx.CollectableRow) error, sql string,
	args ...any,
) error {
	s := &stmt{sql: sql, args: args, op: "query"}
	s.scan = func(rows pgx.Rows) error {
		for rows.Next() {
			if err := fn(rows); err != nil {
				return err
			}
		}
		return nil
	}
	return runRaw(ctx, db, []*stmt{s})
}

// QueryAll runs one SQL statement keel does not render, as QueryRows does, and returns every row
// it returns converted by to, such as pgx.RowTo[int64] or pgx.RowToStructByName[T].
func QueryAll[T any](ctx context.Context, db *DB, to pgx.RowToFunc[T], sql string, args ...any) ([]T, error) {
	var out []T
	err := QueryRows(ctx, db, func(row pgx.CollectableRow) error {
		v, err := to(row)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	}, sql, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// QueryValue runs one SQL statement keel does not render, as QueryRows does, and scans the first
// column of its first row into a T. It returns ErrNotFound when the statement returns no row.
func QueryValue[T any](ctx context.Context, db *DB, sql string, args ...any) (T, error) {
	var out T
	s := &stmt{sql: sql, args: args, op: "query"}
	s.scan = func(rows pgx.Rows) error {
		if !rows.Next() {
			return ErrNotFound
		}
		return rows.Scan(&out)
	}
	err := runRaw(ctx, db, []*stmt{s})
	return out, err
}

// runRaw runs statements keel does not render in one round trip: in the transaction on the
// context (on its server, read-only or not), else on the primary, in an implicit transaction when
// Config.OnBegin is set.
func runRaw(ctx context.Context, db *DB, stmts []*stmt) error {
	mode := modeExec
	if tx := txFrom(ctx, db); tx != nil && tx.readOnly {
		mode = modePrimaryRead
	}
	run, err := db.runnerFor(ctx, mode)
	if err != nil {
		return err
	}
	defer run.abandon()
	return run.close(ctx, run.batch(ctx, stmts, true))
}

// ExecAll runs one statement keel does not render on the primary and on every replica, outside
// any transaction on the context, and returns the rows it affected on all of them together. It is
// for statements about the servers rather than the data, which must reach whichever server a
// backend runs on: cancelling the backends tagged with an application_name, for one. Every server
// is tried; the errors of those that fail are joined. Observers see one statement per server with
// the Op "exec"; the replica's events carry its name.
func ExecAll(ctx context.Context, db *DB, sql string, args ...any) (int64, error) {
	n, err := db.execOn(ctx, db.pool, nil, sql, args)
	if db.replicas == nil {
		return n, err
	}
	errs := []error{err}
	for _, rep := range db.replicas.list {
		m, rerr := db.execOn(ctx, rep.pool, rep, sql, args)
		n += m
		errs = append(errs, rerr)
	}
	return n, errors.Join(errs...)
}

// execOn runs a statement on a pool, the primary's when rep is nil, for its observers.
func (db *DB) execOn(ctx context.Context, pool *pgxpool.Pool, rep *replica, sql string, args []any) (int64, error) {
	s := &stmt{sql: sql, args: args, op: "exec"}
	events, qctx := db.startEvents(ctx, rep, false, []*stmt{s})
	start := time.Now()
	tag, err := pool.Exec(qctx, sql, args...)
	s.tag = tag
	err = db.endEvents(events, []*stmt{s}, time.Since(start), err)
	if err != nil && rep != nil {
		err = fmt.Errorf("keel: replica %s: %w", rep.name, err)
	}
	return tag.RowsAffected(), err
}

// JSON returns the JSON implementation db encodes and decodes json and jsonb values with:
// Config.JSON, or encoding/json when it is unset, with UnmarshalNumber always set (encoding/json's
// Decoder with UseNumber when the configuration leaves it nil). Packages built on keel use it to
// read JSON the way the application configured.
func (db *DB) JSON() JSONCodec {
	c := JSONCodec{Marshal: json.Marshal, Unmarshal: json.Unmarshal}
	if cfg := db.cfg.JSON; cfg != nil && cfg.Marshal != nil && cfg.Unmarshal != nil {
		c = *cfg
	}
	if c.UnmarshalNumber == nil {
		c.UnmarshalNumber = UnmarshalJSONNumber
	}
	return c
}
