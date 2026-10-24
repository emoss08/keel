// Package rollup maintains pre-aggregated tables for keel/analytics and routes reports to them.
//
// A rollup is declared once from the generated column handles:
//
//	s := fleetdb.Shipments
//	var ShipmentsDaily = rollup.MustDefine("shipments_daily", fleetdb.ShipmentsSchema(),
//	    rollup.Day(s.CreatedAt),
//	    rollup.Dims(s.CustomerID, s.Status),
//	    rollup.Measures(rollup.Count(), rollup.Sum(s.Total), rollup.Avg(s.Weight)))
//
// Everything else is derived from that declaration and the schema: the rollup table, whose column
// types Postgres takes from the source table; the migration that creates it with the statement
// triggers that mark the (tenant, bucket) slices every write touches; incremental refresh of those
// slices; and the routing that answers a covered report from the rollup instead of the source.
package rollup

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"time"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// rollupName is what a rollup name may be: it names the rollup table and prefixes its function
// and triggers, which must fit Postgres's 63-byte identifiers.
var rollupName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// colRef is a column taken from a generated handle, or the error of taking it.
type colRef struct {
	ref   *keel.Ref
	index int32
	err   error
}

// colType is the type every generated column kind embeds.
var colType = reflect.TypeFor[keel.Col]()

// columnOf takes the keel.Col a generated column handle embeds, at any depth of embedding.
func columnOf(e keel.Expr) colRef {
	if e == nil {
		return colRef{err: errors.New("rollup: a column is nil")}
	}
	v := reflect.ValueOf(e)
	for {
		if v.Type() == colType {
			c := v.Interface().(keel.Col)
			if c.Ref == nil || c.Ref.Table == nil {
				return colRef{err: errors.New("rollup: a column has no table")}
			}
			return colRef{ref: c.Ref, index: c.Index}
		}
		if v.Kind() != reflect.Struct || v.NumField() == 0 || !v.Type().Field(0).Anonymous {
			return colRef{err: fmt.Errorf("rollup: %T is not a generated column", e)}
		}
		v = v.Field(0)
	}
}

// Grain is the period a rollup aggregates a date, time or epoch column to.
type Grain struct {
	col  colRef
	unit ir.Bucket
}

// Hour aggregates to the hour of c.
func Hour(c keel.Expr) Grain { return Grain{col: columnOf(c), unit: ir.BucketHour} }

// Day aggregates to the day of c.
func Day(c keel.Expr) Grain { return Grain{col: columnOf(c), unit: ir.BucketDay} }

// Week aggregates to the ISO week of c, starting Monday.
func Week(c keel.Expr) Grain { return Grain{col: columnOf(c), unit: ir.BucketWeek} }

// Month aggregates to the month of c.
func Month(c keel.Expr) Grain { return Grain{col: columnOf(c), unit: ir.BucketMonth} }

// Quarter aggregates to the quarter of c.
func Quarter(c keel.Expr) Grain { return Grain{col: columnOf(c), unit: ir.BucketQuarter} }

// Year aggregates to the year of c.
func Year(c keel.Expr) Grain { return Grain{col: columnOf(c), unit: ir.BucketYear} }

// Dim is a column a rollup groups by.
type Dim struct{ col colRef }

// Dims returns the dimensions for columns.
func Dims(cols ...keel.Expr) []Dim {
	out := make([]Dim, len(cols))
	for i, c := range cols {
		out[i] = Dim{col: columnOf(c)}
	}
	return out
}

// Measure is an aggregate a rollup stores. Sums, counts, minimums and maximums compose, so a
// report over coarser buckets or fewer dimensions re-aggregates them; an average is stored as a sum
// and a count; a distinct count answers only reports at the rollup's exact grain and dimensions.
type Measure struct {
	agg  ir.Agg
	col  colRef
	rows bool
}

// Count counts rows.
func Count() Measure { return Measure{agg: ir.AggCount, rows: true} }

// CountOf counts the non-null values of c.
func CountOf(c keel.Expr) Measure { return Measure{agg: ir.AggCount, col: columnOf(c)} }

// Sum totals c.
func Sum(c keel.Expr) Measure { return Measure{agg: ir.AggSum, col: columnOf(c)} }

// Avg averages c, stored as its sum and its count.
func Avg(c keel.Expr) Measure { return Measure{agg: ir.AggAvg, col: columnOf(c)} }

// Min takes the smallest value of c.
func Min(c keel.Expr) Measure { return Measure{agg: ir.AggMin, col: columnOf(c)} }

// Max takes the largest value of c.
func Max(c keel.Expr) Measure { return Measure{agg: ir.AggMax, col: columnOf(c)} }

// CountDistinct counts the distinct non-null values of c.
func CountDistinct(c keel.Expr) Measure { return Measure{agg: ir.AggCountDistinct, col: columnOf(c)} }

// Measures returns ms, for a declaration that reads as a list.
func Measures(ms ...Measure) []Measure { return ms }

// Option configures a rollup.
type Option func(*options)

// options are a rollup's settings beyond its shape.
type options struct {
	zone        string
	sourceIndex bool
}

// Zone computes the rollup's buckets in the IANA time zone name instead of UTC. A report is
// answered from the rollup only when it runs in the same zone.
func Zone(name string) Option { return func(o *options) { o.zone = name } }

// WithoutSourceIndex leaves out of the migration the index on the source table's tenant and grain
// columns that refresh reads through, for a table that already has one with those leading columns.
func WithoutSourceIndex() Option { return func(o *options) { o.sourceIndex = false } }

// Rollup is a declared rollup: a source table aggregated by tenant, grain bucket and dimensions.
type Rollup struct {
	name        string
	source      *schema.Table
	grain       int32
	grainType   schema.ValueType
	unit        ir.Bucket
	dims        []int32
	measures    []resolvedMeasure
	zoneName    string
	zone        *time.Location
	sourceIndex bool
	cols        []column
	table       *schema.Table
}

// resolvedMeasure is a declared measure on a source column index, -1 for Count.
type resolvedMeasure struct {
	agg ir.Agg
	col int32
}

// MustDefine is Define for package-level declarations: it panics on an invalid rollup.
func MustDefine(name string, table *schema.Table, grain Grain, dims []Dim, measures []Measure,
	opts ...Option,
) *Rollup {
	r, err := Define(name, table, grain, dims, measures, opts...)
	if err != nil {
		panic(err)
	}
	return r
}

// Define declares a rollup of table, checked against its schema: every column belongs to table,
// the grain is a date, time or epoch column, dimensions are groupable non-tenant columns, sums and
// averages read numbers, minimums and maximums ordered values, and the derived column names do not
// collide.
func Define(name string, table *schema.Table, grain Grain, dims []Dim, measures []Measure,
	opts ...Option,
) (*Rollup, error) {
	o := options{zone: "UTC", sourceIndex: true}
	for _, opt := range opts {
		opt(&o)
	}
	r := &Rollup{name: name, source: table, unit: grain.unit, zoneName: o.zone, sourceIndex: o.sourceIndex}
	if err := r.check(grain, dims, measures); err != nil {
		return nil, fmt.Errorf("rollup: %s: %w", name, err)
	}
	if err := r.derive(); err != nil {
		return nil, fmt.Errorf("rollup: %s: %w", name, err)
	}
	return r, nil
}

// check validates and resolves the declaration into r.
func (r *Rollup) check(grain Grain, dims []Dim, measures []Measure) error {
	if !rollupName.MatchString(r.name) {
		return fmt.Errorf("the name must match %s", rollupName)
	}
	if r.source == nil {
		return errors.New("the table is nil")
	}
	loc, err := time.LoadLocation(r.zoneName)
	if err != nil {
		return fmt.Errorf("time zone %q: %w", r.zoneName, err)
	}
	r.zone = loc
	var base *keel.Ref
	if r.grain, err = r.own(grain.col, &base); err != nil {
		return fmt.Errorf("grain: %w", err)
	}
	if err := r.checkGrain(); err != nil {
		return err
	}
	for _, d := range dims {
		idx, err := r.own(d.col, &base)
		if err != nil {
			return fmt.Errorf("dimension: %w", err)
		}
		if err := r.checkDim(idx); err != nil {
			return err
		}
		r.dims = append(r.dims, idx)
	}
	if len(measures) == 0 {
		return errors.New("a rollup needs at least one measure")
	}
	for _, m := range measures {
		rm, err := r.checkMeasure(m, &base)
		if err != nil {
			return err
		}
		if !slices.Contains(r.measures, rm) {
			r.measures = append(r.measures, rm)
		}
	}
	return nil
}

// own returns the index of c, checking it is a column of the source table taken from the same
// handle as the columns before it, not through a relation.
func (r *Rollup) own(c colRef, base **keel.Ref) (int32, error) {
	if c.err != nil {
		return -1, c.err
	}
	if c.ref.Table != r.source {
		return -1, fmt.Errorf("%s.%s is not a column of %s", c.ref.Table.Name,
			c.ref.Table.Columns[c.index].Name, r.source.Name)
	}
	if *base == nil {
		*base = c.ref
	} else if *base != c.ref {
		return -1, fmt.Errorf("%s is taken from another handle of %s, such as a relation or an alias",
			r.source.Columns[c.index].Name, r.source.Name)
	}
	return c.index, nil
}

// checkGrain checks that the grain column holds dates, times or epochs, and that a date grain is a
// day or coarser.
func (r *Rollup) checkGrain() error {
	c := &r.source.Columns[r.grain]
	r.grainType = valueType(c)
	switch r.grainType {
	case schema.ValueTime, schema.ValueEpoch:
	case schema.ValueDate:
		if r.unit == ir.BucketHour {
			return fmt.Errorf("grain: %s holds dates, which have no hours", c.Name)
		}
	default:
		return fmt.Errorf("grain: %s holds neither dates, times nor epochs", c.Name)
	}
	if !slices.Contains(ir.Buckets(), r.unit) {
		return fmt.Errorf("grain: unknown unit %q", r.unit)
	}
	return nil
}

// checkDim checks that column idx can be a dimension.
func (r *Rollup) checkDim(idx int32) error {
	c := &r.source.Columns[idx]
	switch {
	case c.Flags.Has(schema.FlagTenant):
		return fmt.Errorf("dimension %s is a tenant column, which every rollup already keeps", c.Name)
	case idx == r.grain:
		return fmt.Errorf("dimension %s is the grain column", c.Name)
	case slices.Contains(r.dims, idx):
		return fmt.Errorf("dimension %s is listed twice", c.Name)
	case c.Kind == schema.KindTSVector, c.Kind == schema.KindJSON && !c.Flags.Has(schema.FlagJSONB):
		return fmt.Errorf("dimension %s cannot be grouped by", c.Name)
	}
	return nil
}

// checkMeasure resolves m, checking that its column suits its aggregation.
func (r *Rollup) checkMeasure(m Measure, base **keel.Ref) (resolvedMeasure, error) {
	if m.rows {
		return resolvedMeasure{agg: ir.AggCount, col: -1}, nil
	}
	idx, err := r.own(m.col, base)
	if err != nil {
		return resolvedMeasure{}, fmt.Errorf("measure: %w", err)
	}
	c := &r.source.Columns[idx]
	vt := valueType(c)
	switch {
	case c.Flags.Has(schema.FlagTenant):
		return resolvedMeasure{}, fmt.Errorf("measure %s of %s: a tenant column", m.agg, c.Name)
	case (m.agg == ir.AggSum || m.agg == ir.AggAvg) && !vt.Numeric():
		return resolvedMeasure{}, fmt.Errorf("measure %s of %s: the column does not hold numbers", m.agg, c.Name)
	case (m.agg == ir.AggMin || m.agg == ir.AggMax) && !vt.Ordered():
		return resolvedMeasure{}, fmt.Errorf("measure %s of %s: the column's values are not ordered", m.agg, c.Name)
	}
	return resolvedMeasure{agg: m.agg, col: idx}, nil
}

// valueType returns the report value type of c: its derived report type, or one inferred from its
// kind and name for a model that is not reportable.
func valueType(c *schema.Column) schema.ValueType {
	if c.Report != nil {
		return c.Report.Type
	}
	switch c.Kind {
	case schema.KindTime:
		return schema.ValueTime
	case schema.KindInt64:
		if hasEpochName(c.Name) {
			return schema.ValueEpoch
		}
		return schema.ValueInt
	case schema.KindInt16, schema.KindInt32:
		return schema.ValueInt
	case schema.KindFloat32, schema.KindFloat64:
		return schema.ValueFloat
	case schema.KindString:
		return schema.ValueString
	case schema.KindBool:
		return schema.ValueBool
	}
	return schema.ValueOther
}

// epochName matches the column names keel gen reads as epochs.
var epochName = regexp.MustCompile(`_(at|date|time)$`)

// hasEpochName reports whether an int64 column's name marks it as an epoch.
func hasEpochName(name string) bool { return epochName.MatchString(name) }

// Name returns the rollup's name, which is also its table's.
func (r *Rollup) Name() string { return r.name }

// Source returns the table the rollup aggregates.
func (r *Rollup) Source() *schema.Table { return r.source }

// Table returns the schema of the rollup table: the source's tenant columns, bucket (the start of
// each grain bucket in the rollup's zone), the dimensions, and the stored aggregates. The source's
// belongs-to relations whose keys are dimensions are carried over, so reports can group by the
// related rows' fields.
func (r *Rollup) Table() *schema.Table { return r.table }

// Unit returns the grain's period.
func (r *Rollup) Unit() ir.Bucket { return r.unit }

// GrainColumn returns the source column the grain buckets.
func (r *Rollup) GrainColumn() *schema.Column { return &r.source.Columns[r.grain] }

// Location returns the time zone buckets are computed in.
func (r *Rollup) Location() *time.Location { return r.zone }

// Dimensions returns the source columns the rollup groups by.
func (r *Rollup) Dimensions() []*schema.Column {
	out := make([]*schema.Column, len(r.dims))
	for i, d := range r.dims {
		out[i] = &r.source.Columns[d]
	}
	return out
}

// Aggregate is a measure as declared: its aggregation and the source column it reads, nil for
// Count.
type Aggregate struct {
	Agg    ir.Agg
	Column *schema.Column
}

// Aggregates returns the declared measures, each once, in declaration order.
func (r *Rollup) Aggregates() []Aggregate {
	out := make([]Aggregate, len(r.measures))
	for i, m := range r.measures {
		out[i].Agg = m.agg
		if m.col >= 0 {
			out[i].Column = &r.source.Columns[m.col]
		}
	}
	return out
}
