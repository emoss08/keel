package analytics

import (
	"context"
	"errors"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// RollupRouter answers reports from rollup tables: small pre-aggregated tenant tables that keel
// maintains beside the base tables. The compiler asks it about every aggregated report that reads
// no table with row rules; when Route returns a plan, the report's statements read the rollup
// table instead of the base tables, and when the plan does not cover something the report needs,
// the compiler falls back to the base tables. analytics/rollup implements it.
type RollupRouter interface {
	// Route returns the plan that answers the report from a rollup, or false when no rollup
	// covers it: dimensions within the rollup's, filters on its dimensions or grain, measures it
	// can re-aggregate, and fresh slices for the tenant.
	Route(ctx context.Context, resolved *ir.Resolved) (RollupPlan, bool)
}

// RollupPlan tells the compiler how to answer a report from a rollup table.
type RollupPlan struct {
	// Name names the rollup; Compiled.Rollup reports it.
	Name string
	// Table is the rollup table, read through keel with its tenant predicate.
	Table *schema.Table
	// Fields maps each field path the report reads (dimensions, filters, the pivot) to the column
	// of the rollup table that holds its values, in the field's own type. A grain column holds the
	// start of each grain period, so bucketing it to a coarser period gives what bucketing the
	// field gives.
	Fields map[string]string
	// Measures maps each measure column id of the report to how the rollup re-aggregates it.
	Measures map[string]RollupMeasure
}

// RollupMeasure is how a rollup re-aggregates one measure.
type RollupMeasure struct {
	// Agg is the re-aggregation.
	Agg ReAgg
	// Column holds the partial aggregate per rollup row: a sum, a count, a minimum or a maximum.
	Column string
	// Count holds the partial count of an average, which is the sum of Column over the sum of
	// Count.
	Count string
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
	plan, ok := opts.Rollups.Route(ctx, res)
	if !ok || plan.Table == nil {
		return nil, nil
	}
	probe.rollup = &plan
	probe.tables = map[*schema.Table]bool{plan.Table: true}
	out, err := probe.compile()
	if errors.Is(err, errRollupMiss) {
		return nil, nil
	}
	return out, err
}

// rollupField reads a field from the rollup table.
func (c *compiler) rollupField(q *keel.DynQuery, path string) (keel.DynExpr, error) {
	col, ok := c.rollup.Fields[path]
	if !ok {
		return keel.DynExpr{}, errRollupMiss
	}
	e := q.Path(col)
	if e.Err() != nil {
		return keel.DynExpr{}, errRollupMiss
	}
	return e, nil
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
	x := f.q.Path(rm.Column)
	if x.Err() != nil {
		return keel.DynExpr{}, errRollupMiss
	}
	t := c.preTransform(p)
	switch rm.Agg {
	case ReSum:
		e := aggregate(ir.AggSum, x, filter, t)
		if p.def.Measure.Agg == ir.AggCount || p.def.Measure.Agg == ir.AggCountDistinct {
			return coalesceZero(aggregate(ir.AggSum, x, filter, formula.Int)), nil
		}
		return e, nil
	case ReMin:
		return aggregate(ir.AggMin, x, filter, t), nil
	case ReMax:
		return aggregate(ir.AggMax, x, filter, t), nil
	case ReAvg:
		n := f.q.Path(rm.Count)
		if n.Err() != nil {
			return keel.DynExpr{}, errRollupMiss
		}
		total := aggregate(ir.AggSum, x, filter, formula.Decimal)
		count := aggregate(ir.AggSum, n, filter, formula.Decimal)
		if t == formula.Float {
			return total.Cast(keel.TypeFloat64).Div(count.NullIf(0)), nil
		}
		return total.Cast(keel.TypeNumeric).Div(count.NullIf(0)), nil
	}
	return keel.DynExpr{}, errRollupMiss
}
