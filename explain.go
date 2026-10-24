package keel

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// ExplainOptions configures Explain.
type ExplainOptions struct {
	// Analyze runs the statement (EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)) to report actual rows,
	// timings and buffer use. It runs inside a transaction that is rolled back, or a savepoint
	// rolled back to when the context carries a transaction, so nothing the statement does is
	// kept.
	Analyze bool
}

// Plan is the plan Postgres chose for a statement: the planner's estimates, the measured values
// when Explain analyzed it, and the whole plan as EXPLAIN's JSON.
type Plan struct {
	// TotalCost is the planner's estimated cost of the whole statement, in its cost units.
	TotalCost float64
	// Rows is the planner's estimate of the rows the statement returns.
	Rows float64
	// ActualRows is the number of rows the statement returned; set only with Analyze, as are the
	// other measured fields.
	ActualRows float64
	// ActualTime is the top plan node's total time in milliseconds.
	ActualTime float64
	// PlanningTime is the time spent planning, in milliseconds.
	PlanningTime float64
	// ExecutionTime is the time spent executing, in milliseconds.
	ExecutionTime float64
	// JSON is EXPLAIN's output.
	JSON []byte
}

// Explain returns the plan of the query's main statement, rendered as ToSQL renders it under
// ctx's scope, with its parameters bound so the plan is the one Postgres chooses for these values.
func (q *Query[T]) Explain(ctx context.Context, db *DB, opts ExplainOptions) (*Plan, error) {
	sql, args, err := q.ToSQL(ctx, db)
	if err != nil {
		return nil, err
	}
	ctx, cancel := q.q.checkTimeout(ctx)
	defer cancel()
	return db.explain(ctx, sql, args, opts)
}

// Explain returns the plan of the dynamic query's statement, as Query.Explain does: an analytics
// guard compares TotalCost and Rows with its limits before running a report.
func (q *DynQuery) Explain(ctx context.Context, db *DB, opts ExplainOptions) (*Plan, error) {
	sql, args, err := q.ToSQL(ctx, db)
	if err != nil {
		return nil, err
	}
	ctx, cancel := q.q.checkTimeout(ctx)
	defer cancel()
	return db.explain(ctx, sql, args, opts)
}

// errExplained rolls back the transaction an analyzed statement ran in.
var errExplained = errors.New("keel: explained")

// explain runs EXPLAIN over sql in the transaction on ctx, else in an implicit read-only one with
// the OnBegin statements, and parses its plan.
func (db *DB) explain(ctx context.Context, sql string, args []any, opts ExplainOptions) (*Plan, error) {
	prefix := "EXPLAIN (FORMAT JSON) "
	if opts.Analyze {
		prefix = "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "
	}
	var raw string
	s := &stmt{sql: prefix + sql, args: args, op: "explain", table: "explain"}
	s.scan = func(rows pgx.Rows) error {
		if rows.Next() {
			return rows.Scan(&raw)
		}
		return nil
	}
	var err error
	switch tx := txFrom(ctx, db); {
	case !opts.Analyze:
		run, rerr := db.newRunner(ctx, false)
		if rerr != nil {
			return nil, rerr
		}
		err = run.close(ctx, run.one(ctx, s, true))
	case tx != nil:
		err = db.explainInSavepoint(ctx, tx, s)
	default:
		run := &runner{db: db, sc: db.resolveScope(ctx), own: true,
			tx: &txState{db: db, readOnly: true, implicit: true, scopeCtx: ctx}}
		err = run.batch(ctx, []*stmt{s}, false)
		if err == nil {
			err = errExplained
		}
		if err = run.close(ctx, err); err == errExplained {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	return parsePlan([]byte(raw))
}

// explainInSavepoint runs an analyzed EXPLAIN inside a savepoint of tx and rolls back to it, in
// one round trip.
func (db *DB) explainInSavepoint(ctx context.Context, tx *txState, s *stmt) error {
	tx.savepoints++
	name := "keel_sp_" + strconv.Itoa(tx.savepoints)
	run := &runner{db: db, sc: db.resolveScope(ctx), tx: tx}
	rollback := &stmt{sql: "ROLLBACK TO SAVEPOINT " + name, op: "savepoint"}
	err := run.batch(ctx, []*stmt{
		{sql: "SAVEPOINT " + name, op: "savepoint"},
		s,
		rollback,
		{sql: "RELEASE SAVEPOINT " + name, op: "savepoint"},
	}, false)
	if err != nil {
		if rbErr := run.one(ctx, rollback, false); rbErr != nil {
			return errors.Join(err, rbErr)
		}
	}
	return err
}

// parsePlan reads the estimates and measurements of EXPLAIN's JSON output.
func parsePlan(raw []byte) (*Plan, error) {
	var out []struct {
		Plan struct {
			TotalCost       float64 `json:"Total Cost"`
			PlanRows        float64 `json:"Plan Rows"`
			ActualRows      float64 `json:"Actual Rows"`
			ActualTotalTime float64 `json:"Actual Total Time"`
		} `json:"Plan"`
		PlanningTime  float64 `json:"Planning Time"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errf("parse EXPLAIN output: %w", err)
	}
	if len(out) == 0 {
		return nil, errf("EXPLAIN returned no plan")
	}
	p := out[0]
	return &Plan{
		TotalCost:     p.Plan.TotalCost,
		Rows:          p.Plan.PlanRows,
		ActualRows:    p.Plan.ActualRows,
		ActualTime:    p.Plan.ActualTotalTime,
		PlanningTime:  p.PlanningTime,
		ExecutionTime: p.ExecutionTime,
		JSON:          raw,
	}, nil
}
