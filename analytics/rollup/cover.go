package rollup

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

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
	res *ir.Resolved
	def *ir.Definition
	loc *time.Location
	now time.Time

	plan     *Plan
	grouped  map[int32]bool
	inexact  bool
	grainBy  ir.Bucket
	lo, hi   time.Time
	distinct bool
}

// newCoverer prepares to check ix's rollup against res run in loc at now.
func newCoverer(ix *indexed, res *ir.Resolved, loc *time.Location, now time.Time) *coverer {
	return &coverer{
		ix: ix, r: ix.r, res: res, def: res.Definition, loc: loc, now: now,
		grouped: map[int32]bool{},
		plan: &Plan{
			Rollup: ix.r, Table: ix.r.table, Location: ix.r.zone,
			Fields: map[string]Field{}, Dimensions: map[string]Dimension{}, Measures: map[string]MeasurePlan{},
		},
	}
}

// run checks every part of the report and fills the plan.
func (c *coverer) run() error {
	if !c.res.Aggregated {
		return errors.New("the report lists rows rather than aggregating them")
	}
	if c.loc.String() != c.r.zone.String() {
		return fmt.Errorf("the report runs in %s, the rollup's buckets are in %s", c.loc, c.r.zone)
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
	c.plan.From, c.plan.To, c.plan.exact = c.lo, c.hi, c.distinct
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

// field classifies the field at path and records where it lives. A related row's key, such as
// customer.id, is the source's own foreign key column and needs no join.
func (c *coverer) field(path string) (fieldClass, int32, error) {
	rf := c.res.Fields[path]
	if rf == nil {
		return fieldNone, -1, fmt.Errorf("field %s was not resolved", path)
	}
	if rf.ToMany {
		return fieldNone, -1, fmt.Errorf("field %s crosses a to-many relation", path)
	}
	key := path[strings.LastIndexByte(path, '.')+1:]
	if rf.Edges == "" {
		src, ok := c.ix.fields[key]
		if !ok {
			return fieldNone, -1, fmt.Errorf("field %s is not a column of %s", path, c.r.source.Name)
		}
		return c.own(path, src), src, nil
	}
	rel, ok := c.ix.edges[rf.Edges]
	if !ok {
		return fieldNone, -1, fmt.Errorf("field %s is reached through %s, which the rollup cannot join", path, rf.Edges)
	}
	target, ok := c.ix.targets[rel][key]
	if !ok {
		return fieldNone, -1, fmt.Errorf("field %s is not a column of %s", path, rf.Edges)
	}
	sr := &c.r.source.Relations[rel]
	if len(sr.Remote) == 1 && sr.Remote[0] == target {
		return c.own(path, sr.Local[0]), sr.Local[0], nil
	}
	join, ok := c.ix.joins[rel]
	if !ok {
		return fieldNone, -1, fmt.Errorf("field %s needs %s, whose key is not a dimension of the rollup", path, rf.Edges)
	}
	vt := valueType(&sr.Target.Columns[target])
	c.plan.Fields[path] = Field{Column: -1, Relation: join, Target: target, Source: vt}
	return fieldJoined, -1, nil
}

// own classifies source column src, reached as path, and records where it lives.
func (c *coverer) own(path string, src int32) fieldClass {
	vt := valueType(&c.r.source.Columns[src])
	if src == c.r.grain {
		c.plan.Fields[path] = Field{Column: c.r.bucketIndex(), Relation: -1, Grain: true, Source: vt}
		return fieldGrain
	}
	if idx, ok := c.r.colIndex(src); ok {
		c.plan.Fields[path] = Field{Column: idx, Relation: -1, Source: vt}
		return fieldDim
	}
	return fieldMeasured
}

// dimension checks a dimension column: a rollup dimension, a field joined through one, or the
// grain field bucketed to the grain or a coarser unit.
func (c *coverer) dimension(col *ir.Column) error {
	d := col.Dimension
	class, src, err := c.field(d.Field)
	if err != nil {
		return err
	}
	dim := Dimension{Field: d.Field}
	switch class {
	case fieldGrain:
		switch {
		case d.Band != nil || col.Transform != nil:
			return errors.New("the grain field is banded or transformed")
		case d.Bucket.Param != "":
			return errors.New("the bucket is chosen by a parameter")
		case d.Bucket.Unit == "":
			return errors.New("the grain field is grouped without a bucket")
		case !coarsens(c.r.unit, d.Bucket.Unit):
			return fmt.Errorf("%s buckets are not whole %s buckets", d.Bucket.Unit, c.r.unit)
		}
		if d.Bucket.Unit != c.r.unit {
			dim.Bucket = d.Bucket.Unit
		}
		c.grainBy = d.Bucket.Unit
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
	c.plan.Dimensions[col.ID] = dim
	return nil
}

// measure maps a measure column to stored aggregates.
func (c *coverer) measure(id string, m *ir.Measure) error {
	col := int32(-1)
	if m.Field != "" {
		class, src, err := c.field(m.Field)
		if err != nil {
			return err
		}
		if class == fieldJoined {
			return fmt.Errorf("the rollup stores no aggregate of %s", m.Field)
		}
		col = src
	}
	mp, err := c.reaggregate(m.Agg, col)
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
	c.plan.Measures[id] = mp
	return nil
}

// reaggregate returns how agg of source column col (-1 for rows) is computed from the rollup.
func (c *coverer) reaggregate(agg ir.Agg, col int32) (MeasurePlan, error) {
	need := func(s stored) (Reaggregate, error) {
		idx, ok := c.r.hasStored(s)
		if !ok {
			what := "rows"
			if s.col >= 0 {
				what = c.r.source.Columns[s.col].Name
			}
			return Reaggregate{}, fmt.Errorf("the rollup stores no %s of %s", s.fn, what)
		}
		fn := "sum"
		if s.fn == "min" || s.fn == "max" {
			fn = s.fn
		}
		return Reaggregate{Fn: fn, Column: idx, Name: c.r.cols[idx].name}, nil
	}
	mp := MeasurePlan{Agg: agg}
	var err error
	switch agg {
	case ir.AggCount:
		mp.Integer = true
		mp.Value, err = need(c.r.countOf(col))
	case ir.AggSum, ir.AggMin, ir.AggMax:
		mp.Value, err = need(stored{fn: string(agg), col: col})
	case ir.AggAvg:
		if mp.Value, err = need(stored{fn: "sum", col: col}); err == nil {
			var d Reaggregate
			d, err = need(c.r.countOf(col))
			mp.Divisor = &d
			mp.Float = valueType(&c.r.source.Columns[col]) == schema.ValueFloat
		}
	case ir.AggCountDistinct:
		mp.Integer = true
		mp.Value, err = need(stored{fn: "count_distinct", col: col})
		c.distinct = true
	default:
		err = fmt.Errorf("unknown aggregation %q", agg)
	}
	return mp, err
}

// calc checks a calculated column: one over other columns only is computed above the rollup; a
// row-level one over fields needs every field to be a rollup dimension.
func (c *coverer) calc(id string) error {
	t := c.res.Calcs[id]
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
		class, _, err := c.field(p)
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
		class, _, err := c.field(f.Field)
		if err != nil {
			return err
		}
		switch class {
		case fieldDim, fieldJoined:
			return nil
		case fieldGrain:
			return c.grainCondition(f, conj)
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

// grainCondition checks a condition on the grain field: it must select whole grain buckets, so
// its bounds fall on bucket starts in the rollup's zone.
func (c *coverer) grainCondition(f *ir.Filter, conj bool) error {
	if f.Transform != nil {
		return errors.New("the grain field is transformed")
	}
	if f.Param != "" {
		return errors.New("a condition on the grain field takes a parameter, which may not fall on a bucket start")
	}
	if f.Op == ir.OpIsNull || f.Op == ir.OpIsNotNull {
		return nil
	}
	if unit, ok := calendarUnit(f.Op); ok {
		if !coarsens(c.r.unit, unit) {
			return fmt.Errorf("%s is not whole %s buckets", f.Op, c.r.unit)
		}
		lo, hi := calendarRange(f.Op, c.now, c.loc)
		c.narrow(conj, lo, hi)
		return nil
	}
	lo, hi, err := c.bounds(f)
	if err != nil {
		return err
	}
	for _, t := range []time.Time{lo, hi} {
		if !t.IsZero() && !truncate(t, c.r.unit, c.r.zone).Equal(t) {
			return fmt.Errorf("%s does not fall on a %s bucket start", t.In(c.r.zone).Format(time.RFC3339), c.r.unit)
		}
	}
	c.narrow(conj, lo, hi)
	return nil
}

// bounds returns the half-open range a comparison on the grain field selects; a zero bound is
// open. A date field's gt, lte and eq become ranges of whole days.
func (c *coverer) bounds(f *ir.Filter) (time.Time, time.Time, error) {
	src := valueType(&c.r.source.Columns[c.r.grain])
	at := func(v any) (time.Time, error) {
		t, ok := instant(v, src, c.loc)
		if !ok {
			return time.Time{}, fmt.Errorf("cannot read %v as an instant", v)
		}
		return t, nil
	}
	nextDay := func(t time.Time) time.Time { return t.In(c.loc).AddDate(0, 0, 1) }
	switch {
	case f.Op == ir.OpGte:
		t, err := at(f.Value)
		return t, time.Time{}, err
	case f.Op == ir.OpLt:
		t, err := at(f.Value)
		return time.Time{}, t, err
	case f.Op == ir.OpBetween:
		vs, ok := f.Value.([]any)
		if !ok || len(vs) != 2 {
			return time.Time{}, time.Time{}, errors.New("between needs two values")
		}
		lo, err := at(vs[0])
		if err != nil {
			return lo, lo, err
		}
		hi, err := at(vs[1])
		return lo, hi, err
	case src == schema.ValueDate && f.Op == ir.OpGt:
		t, err := at(f.Value)
		return nextDay(t), time.Time{}, err
	case src == schema.ValueDate && f.Op == ir.OpLte:
		t, err := at(f.Value)
		return time.Time{}, nextDay(t), err
	case src == schema.ValueDate && f.Op == ir.OpEq:
		t, err := at(f.Value)
		return t, nextDay(t), err
	}
	return time.Time{}, time.Time{}, fmt.Errorf("%s on the grain field does not select whole buckets", f.Op)
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
	class, src, err := c.field(c.def.Pivot.Field)
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
// dimensions, without subtotals, so each of its rows is one rollup row.
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
	}
	return nil
}

// calendarUnit returns the period a relative date operator selects.
func calendarUnit(op ir.Op) (ir.Bucket, bool) {
	switch op {
	case ir.OpToday, ir.OpYesterday, ir.OpTomorrow:
		return ir.BucketDay, true
	case ir.OpThisWeek, ir.OpLastWeek:
		return ir.BucketWeek, true
	case ir.OpThisMonth, ir.OpLastMonth:
		return ir.BucketMonth, true
	case ir.OpThisQuarter, ir.OpLastQuarter:
		return ir.BucketQuarter, true
	case ir.OpThisYear, ir.OpLastYear:
		return ir.BucketYear, true
	}
	return "", false
}

// calendarRange returns the half-open range a relative date operator selects at now in loc.
func calendarRange(op ir.Op, now time.Time, loc *time.Location) (time.Time, time.Time) {
	unit, _ := calendarUnit(op)
	start := truncate(now, unit, loc)
	step := func(t time.Time, n int) time.Time {
		switch unit {
		case ir.BucketDay:
			return t.AddDate(0, 0, n)
		case ir.BucketWeek:
			return t.AddDate(0, 0, 7*n)
		case ir.BucketMonth:
			return t.AddDate(0, n, 0)
		case ir.BucketQuarter:
			return t.AddDate(0, 3*n, 0)
		}
		return t.AddDate(n, 0, 0)
	}
	switch op {
	case ir.OpYesterday, ir.OpLastWeek, ir.OpLastMonth, ir.OpLastQuarter, ir.OpLastYear:
		start = step(start, -1)
	case ir.OpTomorrow:
		start = step(start, 1)
	}
	return start, step(start, 1)
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

// instant reads a filter value on the grain field as an instant: an RFC 3339 time, a local
// date-time or a date in loc, a time.Time, or, for an epoch field, a number of seconds.
func instant(v any, src schema.ValueType, loc *time.Location) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, true
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t, true
		}
		for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, x, loc); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	if src != schema.ValueEpoch {
		return time.Time{}, false
	}
	switch x := v.(type) {
	case json.Number:
		n, err := strconv.ParseInt(string(x), 10, 64)
		return time.Unix(n, 0), err == nil
	case float64:
		return time.Unix(int64(x), 0), x == float64(int64(x))
	case int:
		return time.Unix(int64(x), 0), true
	case int64:
		return time.Unix(x, 0), true
	}
	return time.Time{}, false
}
