package keel

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExplainOptions configures Explain.
type ExplainOptions struct {
	// Analyze runs the statement (EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)) to report actual rows,
	// timings and buffer use. It runs inside a transaction that is rolled back, or a savepoint
	// rolled back to when the context carries a transaction, so nothing the statement does is
	// kept.
	Analyze bool
	// GenericPlan explains the plan Postgres builds without the parameters' values
	// (EXPLAIN (GENERIC_PLAN)), the one a prepared statement settles on after its first runs. A
	// predicate that reaches a partial index or an expression index only through a parameter's
	// value shows here as the scan it really gets. It cannot be combined with Analyze.
	GenericPlan bool
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
	if opts.GenericPlan {
		if opts.Analyze {
			return nil, errf("Explain cannot analyze a generic plan")
		}
		s.sql = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) " + sql
		raw, err := db.explainGeneric(ctx, s)
		if err != nil {
			return nil, err
		}
		return parsePlan([]byte(raw))
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

// explainGeneric runs s, an EXPLAIN (GENERIC_PLAN), on the transaction on ctx, else in a
// read-only one it rolls back, and returns its output. It goes over the simple query protocol
// untouched, so its placeholders stay unbound: the extended protocol would demand their values.
func (db *DB) explainGeneric(ctx context.Context, s *stmt) (string, error) {
	tx := txFrom(ctx, db)
	var run *runner
	if tx != nil {
		var err error
		if run, err = db.txRunner(ctx, tx); err != nil {
			return "", err
		}
	} else {
		run = &runner{db: db, sc: db.resolveScope(ctx), own: true,
			tx: &txState{db: db, readOnly: true, implicit: true, scopeCtx: ctx}}
	}
	var raw string
	conn, err := run.started(ctx)
	if err == nil {
		raw, err = db.simpleQuery(ctx, conn, s)
	}
	if tx != nil {
		return raw, err
	}
	if err == nil {
		err = errExplained
	}
	if err = run.close(ctx, err); err == errExplained {
		err = nil
	}
	return raw, err
}

// simpleQuery sends s over the simple query protocol and returns the first column of its first
// row.
func (db *DB) simpleQuery(ctx context.Context, conn pgConn, s *stmt) (string, error) {
	pc, ok := conn.(interface{ Conn() *pgx.Conn })
	if !ok {
		return "", errf("a generic plan needs a pooled connection")
	}
	events, qctx := db.startEvents(ctx, nil, false, []*stmt{s})
	start := time.Now()
	results, err := pc.Conn().PgConn().Exec(qctx, s.sql).ReadAll()
	var raw string
	if err == nil && len(results) > 0 {
		if err = results[0].Err; err == nil && len(results[0].Rows) > 0 && len(results[0].Rows[0]) > 0 {
			raw = string(results[0].Rows[0][0])
			s.tag = results[0].CommandTag
		}
	}
	return raw, db.endEvents(events, []*stmt{s}, time.Since(start), err)
}

// explainInSavepoint runs an analyzed EXPLAIN inside a savepoint of tx and rolls back to it, in
// one round trip.
func (db *DB) explainInSavepoint(ctx context.Context, tx *txState, s *stmt) error {
	run, err := db.txRunner(ctx, tx)
	if err != nil {
		return err
	}
	tx.savepoints++
	name := "keel_sp_" + strconv.Itoa(tx.savepoints)
	rollback := &stmt{sql: "ROLLBACK TO SAVEPOINT " + name, op: "savepoint"}
	err = run.batch(ctx, []*stmt{
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

// Indexes returns the names of the indexes the plan reads, in plan order, each once.
func (p *Plan) Indexes() []string {
	var out []string
	p.walk(func(n map[string]any) {
		if name, ok := n["Index Name"].(string); ok && !slices.Contains(out, name) {
			out = append(out, name)
		}
	})
	return out
}

// SeqScans returns the tables the plan reads with a sequential scan, in plan order, each once.
func (p *Plan) SeqScans() []string {
	var out []string
	p.walk(func(n map[string]any) {
		if n["Node Type"] != "Seq Scan" {
			return
		}
		if name, ok := n["Relation Name"].(string); ok && !slices.Contains(out, name) {
			out = append(out, name)
		}
	})
	return out
}

// walk calls fn for every node of the plan, parents first.
func (p *Plan) walk(fn func(map[string]any)) {
	var out []struct {
		Plan map[string]any `json:"Plan"`
	}
	if json.Unmarshal(p.JSON, &out) != nil || len(out) == 0 {
		return
	}
	var visit func(n map[string]any)
	visit = func(n map[string]any) {
		fn(n)
		children, _ := n["Plans"].([]any)
		for _, c := range children {
			if m, ok := c.(map[string]any); ok {
				visit(m)
			}
		}
	}
	visit(out[0].Plan)
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
