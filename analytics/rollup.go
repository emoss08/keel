package analytics

import (
	"context"
	"errors"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// RollupRouter answers reports from rollup tables: small pre-aggregated tenant tables that keel
// maintains beside the base tables. The compiler asks it about every aggregated report that reads
// no table with row rules; when Route returns a plan, the report's statements read the rollup
// table instead of the base tables, and when the plan does not cover something the report needs,
// the compiler falls back to the base tables. A Runner then checks the plan's freshness in the
// run's own transaction and reads the base tables when the rollup is stale. analytics/rollup
// implements it: RunnerOptions{Rollups: rollup.NewRouter(rollups...)}.
type RollupRouter interface {
	// Check reports why the router cannot answer reports of the catalog, such as a rollup of a
	// table the catalog does not report on; NewRunner returns it as a CodeCatalog error.
	Check(cat *Catalog) error
	// Route returns the plan that answers the report from a rollup, or false when no rollup
	// covers it: dimensions within the rollup's, filters on its dimensions or whole grain
	// buckets, and measures it can re-aggregate.
	Route(ctx context.Context, q RollupQuery) (*RollupPlan, bool)
}

// RollupQuery is what a RollupRouter routes: a resolved report with what decides its coverage.
type RollupQuery struct {
	// Catalog is the catalog the report was resolved against; field paths resolve through it to
	// the source columns and relations a rollup is declared over.
	Catalog *Catalog
	// Resolved is the report, validated against the catalog.
	Resolved *ir.Resolved
	// Params are the report's bound parameter values, so conditions bound to parameters can be
	// checked against the rollup's buckets.
	Params map[string]any
	// Location is the report's time zone; a rollup answers only reports in its own zone.
	Location *time.Location
	// Now is the instant relative date filters count from.
	Now time.Time
}

// RollupPlan tells the compiler how to answer a report from a rollup table and the runner how to
// check that the rollup is fresh enough to.
type RollupPlan struct {
	// Name names the rollup; Compiled.Rollup and Result.Rollup report it.
	Name string
	// Table is the rollup table, read through keel with its tenant predicate.
	Table *schema.Table
	// Unit is the grain of the rollup's bucket column, and Location the zone its buckets are
	// computed in, which is the report's.
	Unit     ir.Bucket
	Location *time.Location
	// Fields maps each field path the report's dimensions, filters, calculations and pivot read
	// to where the rollup table holds it.
	Fields map[string]RollupField
	// Measures maps each measure column id of the report to how the rollup re-aggregates it.
	Measures map[string]RollupMeasure
	// From and To bound the grain buckets the report reads, [From, To); zero when unbounded.
	From time.Time
	To   time.Time
	// Fresh reports whether the rollup is built and holds no slice of the context's tenant
	// pending refresh within [From, To). The runner calls it in the run's transaction, so a
	// report read from a replica checks the replica it reads, in the same snapshot; false makes
	// the report read the base tables, and an error is reported to OnError and does the same.
	// Nil is always fresh.
	Fresh func(ctx context.Context, db *keel.DB) (bool, error)
}

// RollupField is where the rollup table holds a field of the report.
type RollupField struct {
	// Path is the relation names of the rollup table that lead to the field, then its column:
	// ["customer_id"], or ["Customer", "name"] for a field of a related row.
	Path []string
	// Grain marks the field the rollup buckets: Path names the bucket column, a timestamptz at
	// the start of each Unit bucket in Location. Grouping by the field re-truncates the bucket to
	// the report's unit and returns it in the field's own type (epoch seconds, a date or a
	// timestamp), and filter values written for the field are compared with it as instants.
	Grain bool
}

// RollupMeasure is how a rollup re-aggregates one measure from its stored partial aggregates.
type RollupMeasure struct {
	// Agg is the re-aggregation.
	Agg ReAgg
	// Value is the column holding the partial aggregate per rollup row (a sum, a count, a
	// minimum, a maximum or an exact distinct count), or an average's partial sum.
	Value string
	// Divisor is the column holding an average's partial count; the average is the sum of Value
	// over the sum of Divisor.
	Divisor string
}

// ReAgg is how partial aggregates combine.
type ReAgg uint8

// The re-aggregations.
const (
	// ReSum adds partial sums and counts.
	ReSum ReAgg = iota + 1
	// ReMin takes the smallest partial minimum.
	ReMin
	// ReMax takes the largest partial maximum.
	ReMax
	// ReAvg divides the sum of partial sums by the sum of partial counts.
	ReAvg
)

// errRollupMiss reports that a rollup plan does not cover something the report needs; the
// compiler falls back to the base tables.
var errRollupMiss = errors.New("analytics: the rollup does not cover the report")

// tryRollup compiles the report from the router's rollup when it covers it, and returns nil
// otherwise.
func tryRollup(ctx context.Context, cat *Catalog, dec *decisions, res *ir.Resolved, params map[string]any,
	opts CompileOptions,
) (*Compiled, error) {
	if opts.Rollups == nil || !res.Aggregated {
		return nil, nil
	}
	probe, err := newCompiler(ctx, cat, dec, res, params, opts)
	if err != nil {
		return nil, err
	}
	tables := make([]*schema.Table, 0, len(probe.tables))
	for t := range probe.tables {
		tables = append(tables, t)
	}
	if dec.hasRules(ctx, tables) || probe.hasFanout() {
		return nil, nil
	}
	plan, ok := opts.Rollups.Route(ctx, RollupQuery{Catalog: cat, Resolved: res, Params: params,
		Location: probe.loc, Now: probe.now})
	if !ok || plan == nil || plan.Table == nil {
		return nil, nil
	}
	probe.rollup = plan
	out, err := probe.compile()
	if errors.Is(err, errRollupMiss) {
		return nil, nil
	}
	return out, err
}

// rollupField reads a field from the rollup table.
func (c *compiler) rollupField(q *keel.DynQuery, path string) (keel.DynExpr, error) {
	rf, ok := c.rollup.Fields[path]
	if !ok || len(rf.Path) == 0 {
		return keel.DynExpr{}, errRollupMiss
	}
	e := q.Path(rf.Path...)
	if e.Err() != nil {
		return keel.DynExpr{}, errRollupMiss
	}
	return e, nil
}

// grainField reports whether path is the field the answering rollup buckets.
func (c *compiler) grainField(path string) bool {
	return c.rollup != nil && c.rollup.Fields[path].Grain
}

// rollupBucket groups by the rollup's bucket column: truncated again to unit when it is coarser
// than the rollup's grain, in the report's zone, and returned in t, the type of the field the
// rollup buckets, as bucketing that field returns it.
func (c *compiler) rollupBucket(e keel.DynExpr, t formula.Type, unit ir.Bucket) keel.DynExpr {
	if unit != c.rollup.Unit {
		e = keel.DynOf(keel.Raw("date_trunc(?, ?, ?)", lit(string(unit), formula.String), e, c.tz))
	}
	switch t {
	case formula.Epoch:
		return keel.DynOf(keel.Raw("CAST(extract(epoch from ?) AS bigint)", e))
	case formula.Date:
		return keel.DynOf(keel.Raw("CAST((? AT TIME ZONE ?) AS date)", e, c.tz))
	}
	return e
}

// rollupMeasure re-aggregates a measure's partial aggregates, with the measure's filter and the
// pivot cell's condition on the rollup's dimensions.
func (c *compiler) rollupMeasure(f *frame, p *colPlan, pivot keel.Pred) (keel.DynExpr, error) {
	rm, ok := c.rollup.Measures[p.def.ID]
	if !ok {
		return keel.DynExpr{}, errRollupMiss
	}
	at := "/columns/" + itoa(c.indexOf(p)) + "/measure/filter"
	filter, err := c.filterPred(p.def.Measure.Filter, at, f.whereEnv())
	if err != nil {
		return keel.DynExpr{}, err
	}
	filter = andPred(filter, pivot)
	x := f.q.Path(rm.Value)
	if x.Err() != nil {
		return keel.DynExpr{}, errRollupMiss
	}
	t := c.preTransform(p)
	switch rm.Agg {
	case ReSum:
		if p.def.Measure.Agg == ir.AggCount || p.def.Measure.Agg == ir.AggCountDistinct {
			return coalesceZero(aggregate(ir.AggSum, x, filter, formula.Int)), nil
		}
		return aggregate(ir.AggSum, x, filter, t), nil
	case ReMin:
		return aggregate(ir.AggMin, x, filter, t), nil
	case ReMax:
		return aggregate(ir.AggMax, x, filter, t), nil
	case ReAvg:
		n := f.q.Path(rm.Divisor)
		if n.Err() != nil {
			return keel.DynExpr{}, errRollupMiss
		}
		return mean(aggregate(ir.AggSum, x, filter, formula.Decimal), aggregate(ir.AggSum, n, filter, formula.Decimal), t), nil
	}
	return keel.DynExpr{}, errRollupMiss
}

// rollupCheck is a failed freshness check of a rollup plan; the runner reports it and runs the
// report from the base tables in a new transaction, since the failed check ended the first.
type rollupCheck struct{ err error }

// Error returns the check's error.
func (e *rollupCheck) Error() string { return "analytics: rollup freshness: " + e.err.Error() }

// Unwrap returns the check's error.
func (e *rollupCheck) Unwrap() error { return e.err }

// fresh checks the freshness of the rollup answering comp in the run's transaction and returns
// comp, or the report compiled from the base tables when the rollup is stale.
func (r *Runner) fresh(ctx context.Context, comp *Compiled) (*Compiled, error) {
	if comp.plan == nil || comp.plan.Fresh == nil {
		return comp, nil
	}
	ok, err := comp.plan.Fresh(ctx, r.db)
	if err != nil {
		return nil, &rollupCheck{err: err}
	}
	if ok {
		return comp, nil
	}
	return comp.withoutRollup(ctx)
}

// withoutRollup compiles the report again from the base tables.
func (c *Compiled) withoutRollup(ctx context.Context) (*Compiled, error) {
	opts := c.opts
	opts.Rollups = nil
	return compileWith(ctx, c.cat, c.dec, c.Resolved, c.Params, opts)
}
