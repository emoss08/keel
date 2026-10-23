package keel

import (
	"context"
	"time"
)

// QueryEvent describes one statement. SQL never contains values; Args is the number of bound
// parameters.
type QueryEvent struct {
	Op       string
	Table    string
	SQL      string
	Args     int
	Start    time.Time
	Duration time.Duration
	Rows     int64
	Err      error
}

// Observer sees every statement keel sends: queries, relation loads, writes, COPY, savepoints and
// advisory locks, but not BEGIN, the Config.OnBegin statements, plan-mode switches or COMMIT.
//
// For each statement keel calls every observer's BeforeQuery in order, the first with the
// terminal's context and each later one with the context the previous one returned. The context
// the last one returns is the statement's context: AfterQuery gets it, observers in reverse order,
// so an observer can start a span in BeforeQuery and end it in AfterQuery. A round trip that
// carries one statement runs under that statement's context, so a deadline, cancellation or value
// an observer adds reaches the execution. Statements sent in one round trip (a Batch, the
// statements of one level of relation loads, the chunks of a CreateMany) each start from the
// terminal's context, so their contexts are siblings; the round trip itself runs under the
// terminal's context, and the statements share a start time and a duration.
type Observer interface {
	BeforeQuery(ctx context.Context, e *QueryEvent) context.Context
	AfterQuery(ctx context.Context, e *QueryEvent)
}

// observed is one statement's event and the context its observers returned.
type observed struct {
	e   *QueryEvent
	ctx context.Context
}

// startEvents runs the observers' BeforeQuery for each statement, each starting from ctx. It
// returns the events and the context the round trip runs under: the statement's own context when
// there is one statement, else ctx.
func (db *DB) startEvents(ctx context.Context, stmts []*stmt) ([]observed, context.Context) {
	if len(db.cfg.Observers) == 0 {
		return nil, ctx
	}
	now := time.Now()
	events := make([]observed, len(stmts))
	for i, s := range stmts {
		e := &QueryEvent{Op: s.op, Table: s.table, SQL: s.sql, Args: len(s.args), Start: now}
		sctx := ctx
		for _, o := range db.cfg.Observers {
			sctx = o.BeforeQuery(sctx, e)
		}
		events[i] = observed{e: e, ctx: sctx}
	}
	if len(events) == 1 {
		return events, events[0].ctx
	}
	return events, ctx
}

// endEvents completes the events of one round trip and runs the observers' AfterQuery, each
// statement with its own context.
func (db *DB) endEvents(events []observed, stmts []*stmt, d time.Duration, err error) {
	for i, ev := range events {
		e := ev.e
		e.Duration = d
		e.Err = err
		e.Rows = stmts[i].tag.RowsAffected()
		for j := len(db.cfg.Observers) - 1; j >= 0; j-- {
			db.cfg.Observers[j].AfterQuery(ev.ctx, e)
		}
	}
}
