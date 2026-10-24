package rollup

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/emoss08/keel/analytics"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// fieldClass is what a report field is to a rollup.
type fieldClass uint8

const (
	fieldNone fieldClass = iota
	fieldDim
	fieldGrain
	fieldJoined
	fieldMeasured
)

// coverer decides whether one rollup covers one report, building its plan on the way.
type coverer struct {
	ix  *indexed
	r   *Rollup
	q   analytics.RollupQuery
	def *ir.Definition

	plan     *analytics.RollupPlan
	grouped  map[int32]bool
	inexact  bool
	grainBy  ir.Bucket
	lo, hi   time.Time
	distinct bool
}

// newCoverer prepares to check ix's rollup against q's report.
func newCoverer(ix *indexed, q analytics.RollupQuery) *coverer {
	return &coverer{
		ix: ix, r: ix.r, q: q, def: q.Resolved.Definition, grouped: map[int32]bool{},
		plan: &analytics.RollupPlan{
			Name: ix.r.name, Table: ix.r.table, Unit: ix.r.unit, Location: ix.r.zone,
			Fields: map[string]analytics.RollupField{}, Measures: map[string]analytics.RollupMeasure{},
		},
	}
}

// run checks every part of the report and fills the plan.
func (c *coverer) run() error {
	if !c.q.Resolved.Aggregated {
		return errors.New("the report lists rows rather than aggregating them")
	}
	if c.q.Location.String() != c.r.zone.String() {
		return fmt.Errorf("the report runs in %s, the rollup's buckets are in %s", c.q.Location, c.r.zone)
	}
	for i := range c.def.Columns {
		if err := c.column(&c.def.Columns[i]); err != nil {
			return fmt.Errorf("column %s: %w", c.def.Columns[i].ID, err)
		}
	}
	if err := c.filter(c.def.Filters, true); err != nil {
		return fmt.Errorf("filters: %w", err)
	}
	if err := c.pivot(); err != nil {
		return fmt.Errorf("pivot: %w", err)
	}
	if c.distinct {
		if err := c.exactGrain(); err != nil {
			return fmt.Errorf("count distinct: %w", err)
		}
	}
	c.plan.From, c.plan.To = c.lo, c.hi
	c.plan.Fresh = c.r.freshness(c.lo, c.hi, c.distinct)
	return nil
}

// column checks one report column.
func (c *coverer) column(col *ir.Column) error {
	switch {
	case col.Dimension != nil:
		return c.dimension(col)
	case col.Measure != nil:
		return c.measure(col.ID, col.Measure)
	case col.Calc != nil:
		return c.calc(col.ID)
	}
	return nil
}

// field classifies the field at path, resolved through the catalog, and records where the rollup
// table holds it. A related row's key, such as customer.id, is the source's own foreign key and
// needs no join.
func (c *coverer) field(path string) (fieldClass, int32, *analytics.Field, error) {
	via, f, ok := c.q.Catalog.Resolve(c.def.Entity, path)
	if !ok {
		return fieldNone, -1, nil, fmt.Errorf("field %s is not in the catalog", path)
	}
	switch {
	case len(via) == 0:
		return c.own(path, f.Column), f.Column, f, nil
	case len(via) > 1:
		return fieldNone, -1, f, fmt.Errorf("field %s is reached through %d relations; a rollup joins one", path, len(via))
	case via[0].ToMany:
		return fieldNone, -1, f, fmt.Errorf("field %s crosses a to-many relation", path)
	}
	sr := &c.r.source.Relations[via[0].Relation]
	if sr.Kind != schema.BelongsTo {
		return fieldNone, -1, f, fmt.Errorf("field %s is reached through %s, which the rollup cannot join", path, via[0].Name)
	}
	for i, rc := range sr.Remote {
		if rc == f.Column {
			return c.own(path, sr.Local[i]), sr.Local[i], f, nil
		}
	}
	join, ok := c.ix.joins[via[0].Relation]
	if !ok {
		return fieldNone, -1, f, fmt.Errorf("field %s needs %s, whose key is not a dimension of the rollup", path, via[0].Name)
	}
	rel := &c.r.table.Relations[join]
	c.plan.Fields[path] = analytics.RollupField{Path: []string{rel.Name, rel.Target.Columns[f.Column].Name}}
	return fieldJoined, -1, f, nil
}

// own classifies source column src, reached as path, and records where the rollup table holds it.
func (c *coverer) own(path string, src int32) fieldClass {
	if src == c.r.grain {
		c.plan.Fields[path] = analytics.RollupField{Path: []string{bucketColumn}, Grain: true}
		return fieldGrain
	}
	if idx, ok := c.r.colIndex(src); ok {
		c.plan.Fields[path] = analytics.RollupField{Path: []string{c.r.cols[idx].name}}
		return fieldDim
	}
	return fieldMeasured
}

// dimension checks a dimension column: a rollup dimension, a field joined through one, or the
// grain field bucketed to the grain or a unit made of whole grain buckets.
func (c *coverer) dimension(col *ir.Column) error {
	d := col.Dimension
	class, src, _, err := c.field(d.Field)
	if err != nil {
		return err
	}
	switch class {
	case fieldGrain:
		unit := d.Bucket.Unit
		if d.Bucket.Param != "" {
			s, _ := c.q.Params[d.Bucket.Param].(string)
			unit = ir.Bucket(s)
		}
		switch {
		case d.Band != nil || col.Transform != nil:
			return errors.New("the grain field is banded or transformed")
		case unit == "":
			return errors.New("the grain field is grouped without a bucket")
		case !coarsens(c.r.unit, unit):
			return fmt.Errorf("%s buckets are not whole %s buckets", unit, c.r.unit)
		}
		c.grainBy = unit
	case fieldDim:
		if d.Band != nil || col.Transform != nil || !d.Bucket.IsZero() {
			c.inexact = true
		} else {
			c.grouped[src] = true
		}
	case fieldJoined:
		c.inexact = true
	default:
		return fmt.Errorf("field %s is not a dimension of the rollup", d.Field)
	}
	return nil
}

// measure maps a measure column to stored aggregates.
func (c *coverer) measure(id string, m *ir.Measure) error {
	col := int32(-1)
	if m.Field != "" {
		class, src, _, err := c.field(m.Field)
		if err != nil {
			return err
		}
		if class == fieldJoined {
			return fmt.Errorf("the rollup stores no aggregate of %s", m.Field)
		}
		col = src
	}
	rm, err := c.reaggregate(m.Agg, col)
	if err != nil {
		return err
	}
	if m.Filter != nil {
		if m.Agg == ir.AggCountDistinct {
			return errors.New("a filtered distinct count needs the source rows")
		}
		if err := c.filter(m.Filter, false); err != nil {
			return fmt.Errorf("measure filter: %w", err)
		}
	}
	c.plan.Measures[id] = rm
	return nil
}

// reaggregate returns how agg of source column col (-1 for rows) is computed from the rollup:
// sums and counts are summed, minimums and maximums taken again, an average is a summed sum over
// a summed count, and a distinct count is read at the rollup's exact grain.
func (c *coverer) reaggregate(agg ir.Agg, col int32) (analytics.RollupMeasure, error) {
	need := func(s stored) (string, error) {
		idx, ok := c.r.hasStored(s)
		if !ok {
			what := "rows"
			if s.col >= 0 {
				what = c.r.source.Columns[s.col].Name
			}
			return "", fmt.Errorf("the rollup stores no %s of %s", s.fn, what)
		}
		return c.r.cols[idx].name, nil
	}
	rm := analytics.RollupMeasure{Agg: analytics.ReSum}
	var err error
	switch agg {
	case ir.AggCount:
		rm.Value, err = need(c.r.countOf(col))
	case ir.AggSum:
		rm.Value, err = need(stored{fn: "sum", col: col})
	case ir.AggMin:
		rm.Agg = analytics.ReMin
		rm.Value, err = need(stored{fn: "min", col: col})
	case ir.AggMax:
		rm.Agg = analytics.ReMax
		rm.Value, err = need(stored{fn: "max", col: col})
	case ir.AggAvg:
		rm.Agg = analytics.ReAvg
		if rm.Value, err = need(stored{fn: "sum", col: col}); err == nil {
			rm.Divisor, err = need(c.r.countOf(col))
		}
	case ir.AggCountDistinct:
		rm.Value, err = need(stored{fn: "count_distinct", col: col})
		c.distinct = true
	default:
		err = fmt.Errorf("unknown aggregation %q", agg)
	}
	return rm, err
}

// calc checks a calculated column: one over other columns only is computed above the rollup; a
// row-level one over fields needs every field to be a rollup dimension.
func (c *coverer) calc(id string) error {
	t := c.q.Resolved.Calcs[id]
	if t == nil || t.Node == nil {
		return errors.New("the calculation was not checked")
	}
	refs := t.Node.References(formula.KindField)
	if len(refs) == 0 {
		return nil
	}
	if !t.RowLevel() {
		return errors.New("an aggregate calculation over fields needs the source rows")
	}
	c.inexact = true
	return c.dimensionFields(refs)
}

// dimensionFields checks that every field path is a rollup dimension or joined through one.
func (c *coverer) dimensionFields(paths []string) error {
	for _, p := range paths {
		class, _, _, err := c.field(p)
		if err != nil {
			return err
		}
		if class != fieldDim && class != fieldJoined {
			return fmt.Errorf("field %s is not a dimension of the rollup", p)
		}
	}
	return nil
}

// filter checks a filter tree; conj is true while every group above f is an AND, so a condition
// on the grain field narrows the range the report reads.
func (c *coverer) filter(f *ir.Filter, conj bool) error {
	if f == nil {
		return nil
	}
	if f.IsGroup() {
		for i := range f.Children() {
			if err := c.filter(&f.Children()[i], conj && f.And != nil); err != nil {
				return err
			}
		}
		return nil
	}
	switch {
	case f.Field != "":
		class, _, field, err := c.field(f.Field)
		if err != nil {
			return err
		}
		switch class {
		case fieldDim, fieldJoined:
			return nil
		case fieldGrain:
			return c.grainCondition(f, field.Type, conj)
		}
		return fmt.Errorf("field %s is not a dimension of the rollup", f.Field)
	case f.Calc != nil:
		n := f.Calc.Node
		if n == nil {
			var err error
			if n, err = formula.Parse(f.Calc.Text); err != nil {
				return err
			}
		}
		return c.dimensionFields(n.References(formula.KindField))
	}
	return nil
}

// grainCondition checks a condition on the grain field, whose values are of type t: it must select
// whole grain buckets, so its bounds fall on bucket starts in the rollup's zone.
func (c *coverer) grainCondition(f *ir.Filter, t formula.Type, conj bool) error {
	if f.Transform != nil {
		return errors.New("the grain field is transformed")
	}
	if f.Op == ir.OpIsNull || f.Op == ir.OpIsNotNull {
		return nil
	}
	v := f.Value
	if f.Param != "" {
		var present bool
		if v, present = c.q.Params[f.Param]; !present {
			return nil
		}
	}
	lo, hi, err := c.bounds(f.Op, v, t)
	if err != nil {
		return err
	}
	for _, at := range []time.Time{lo, hi} {
		if !at.IsZero() && !truncate(at, c.r.unit, c.r.zone).Equal(at) {
			return fmt.Errorf("%s does not fall on a %s bucket start", at.In(c.r.zone).Format(time.RFC3339), c.r.unit)
		}
	}
	c.narrow(conj, lo, hi)
	return nil
}

// bounds returns the half-open range a comparison of the grain field with v selects; a zero
// bound is open. A date field's gt, lte and eq are ranges of whole days; relative dates are the
// compiler's ranges.
func (c *coverer) bounds(op ir.Op, v any, t formula.Type) (time.Time, time.Time, error) {
	loc := c.q.Location
	at := func(x any) (time.Time, error) { return analytics.Instant(x, t, loc) }
	nextDay := func(x time.Time) time.Time { return x.In(loc).AddDate(0, 0, 1) }
	switch {
	case op.Relative():
		return analytics.RelativeRange(op, v, c.q.Now, loc)
	case op == ir.OpGte:
		x, err := at(v)
		return x, time.Time{}, err
	case op == ir.OpLt:
		x, err := at(v)
		return time.Time{}, x, err
	case op == ir.OpBetween:
		vs, ok := v.([]any)
		if !ok || len(vs) != 2 {
			return time.Time{}, time.Time{}, errors.New("between needs two values")
		}
		lo, err := at(vs[0])
		if err != nil {
			return lo, lo, err
		}
		hi, err := at(vs[1])
		return lo, hi, err
	case t == formula.Date && op == ir.OpGt:
		x, err := at(v)
		return nextDay(x), time.Time{}, err
	case t == formula.Date && op == ir.OpLte:
		x, err := at(v)
		return time.Time{}, nextDay(x), err
	case t == formula.Date && op == ir.OpEq:
		x, err := at(v)
		return x, nextDay(x), err
	}
	return time.Time{}, time.Time{}, fmt.Errorf("%s on the grain field does not select whole buckets", op)
}

// narrow intersects the read range with [lo, hi) when the condition is on the conjunctive path.
func (c *coverer) narrow(conj bool, lo, hi time.Time) {
	if !conj {
		return
	}
	if !lo.IsZero() && (c.lo.IsZero() || lo.After(c.lo)) {
		c.lo = lo
	}
	if !hi.IsZero() && (c.hi.IsZero() || hi.Before(c.hi)) {
		c.hi = hi
	}
}

// pivot checks the pivot field, which groups like a dimension.
func (c *coverer) pivot() error {
	if c.def.Pivot == nil {
		return nil
	}
	class, src, _, err := c.field(c.def.Pivot.Field)
	if err != nil {
		return err
	}
	switch class {
	case fieldDim:
		c.grouped[src] = true
	case fieldJoined:
		c.inexact = true
	default:
		return fmt.Errorf("field %s is not a dimension of the rollup", c.def.Pivot.Field)
	}
	return nil
}

// exactGrain checks that a report with a distinct count groups by exactly the rollup's grain and
// dimensions, without subtotals or a pivot's Other cell, so each of its cells is one rollup row.
func (c *coverer) exactGrain() error {
	switch {
	case c.grainBy != c.r.unit:
		return fmt.Errorf("the report must group by the %s of the grain field", c.r.unit)
	case c.inexact:
		return errors.New("the report groups by something other than the rollup's dimensions")
	case !slices.Equal(slices.Sorted(maps.Keys(c.grouped)), slices.Sorted(slices.Values(c.r.dims))):
		return errors.New("the report must group by every dimension of the rollup")
	case c.def.Groupings != nil && (len(c.def.Groupings.Subtotals) > 0 || c.def.Groupings.Total):
		return errors.New("subtotals and totals add rows that span several rollup rows")
	case c.def.Pivot != nil && c.def.Pivot.Other:
		return errors.New("a pivot's Other cell spans several rollup rows")
	}
	return nil
}

// truncate returns the start of t's bucket of unit on loc's wall clock, as date_trunc with a time
// zone computes it.
func truncate(t time.Time, unit ir.Bucket, loc *time.Location) time.Time {
	t = t.In(loc)
	y, m, d := t.Date()
	switch unit {
	case ir.BucketHour:
		return time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
	case ir.BucketWeek:
		return time.Date(y, m, d-(int(t.Weekday())+6)%7, 0, 0, 0, 0, loc)
	case ir.BucketMonth:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	case ir.BucketQuarter:
		return time.Date(y, (m-1)/3*3+1, 1, 0, 0, 0, 0, loc)
	case ir.BucketYear:
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	}
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
