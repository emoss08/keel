package keel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
	mode := modeExec
	if tx := txFrom(ctx, db); tx != nil && tx.readOnly {
		mode = modePrimaryRead
	}
	run, err := db.runnerFor(ctx, mode)
	if err != nil {
		return 0, err
	}
	s := &stmt{sql: sql, args: args, op: "exec"}
	if err := run.close(ctx, run.one(ctx, s, true)); err != nil {
		return 0, err
	}
	return s.tag.RowsAffected(), nil
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
	events, qctx := db.startEvents(ctx, rep, []*stmt{s})
	start := time.Now()
	tag, err := pool.Exec(qctx, sql, args...)
	s.tag = tag
	db.endEvents(events, []*stmt{s}, time.Since(start), err)
	if err != nil && rep != nil {
		err = fmt.Errorf("keel: replica %s: %w", rep.name, err)
	}
	return tag.RowsAffected(), err
}

// JSON returns the JSON implementation db encodes and decodes json and jsonb values with:
// Config.JSON, or encoding/json when it is unset. Packages built on keel use it to read JSON the
// way the application configured.
func (db *DB) JSON() JSONCodec {
	if c := db.cfg.JSON; c != nil && c.Marshal != nil && c.Unmarshal != nil {
		return *c
	}
	return JSONCodec{Marshal: json.Marshal, Unmarshal: json.Unmarshal}
}
