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

// Observer sees every statement keel sends. Statements sent in one round trip share a duration.
type Observer interface {
	BeforeQuery(ctx context.Context, e *QueryEvent) context.Context
	AfterQuery(ctx context.Context, e *QueryEvent)
}

func (db *DB) startEvents(ctx context.Context, stmts []*stmt) []*QueryEvent {
	if len(db.cfg.Observers) == 0 {
		return nil
	}
	now := time.Now()
	events := make([]*QueryEvent, len(stmts))
	for i, s := range stmts {
		e := &QueryEvent{Op: s.op, Table: s.table, SQL: s.sql, Args: len(s.args), Start: now}
		for _, o := range db.cfg.Observers {
			ctx = o.BeforeQuery(ctx, e)
		}
		events[i] = e
	}
	return events
}

func (db *DB) endEvents(ctx context.Context, events []*QueryEvent, stmts []*stmt,
	d time.Duration, err error,
) {
	for i, e := range events {
		e.Duration = d
		e.Err = err
		e.Rows = stmts[i].tag.RowsAffected()
		for j := len(db.cfg.Observers) - 1; j >= 0; j-- {
			db.cfg.Observers[j].AfterQuery(ctx, e)
		}
	}
}
