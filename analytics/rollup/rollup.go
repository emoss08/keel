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
	"regexp"
	"slices"
	"strconv"
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
	table *schema.Table
	index int32
	err   error
}

// columnOf takes the table and index of a generated column handle.
func columnOf(e keel.Expr) colRef {
	if e == nil {
		return colRef{err: errors.New("a column is nil")}
	}
	t, i, ok := keel.ColumnOf(e)
	if !ok {
		return colRef{err: fmt.Errorf("%T is not a generated column of a table's own handle (a column taken through a relation belongs to the related row)", e)}
	}
	return colRef{table: t, index: i}
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

	// name, expr and where are set for a measure over an expression: a count of the rows where
	// holds, or a sum of expr, optionally where holds, stored in a column called name.
	name  string
	expr  keel.Expr
	where keel.Pred
	err   error
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

// CountWhere counts the rows where holds, stored in a column called name. Counts compose, so a
// report over coarser buckets or fewer dimensions sums them. where reads only the source table's
// own columns; it may hold values, which the rollup writes as literals.
func CountWhere(name string, where keel.Pred) Measure {
	return Measure{agg: ir.AggCount, name: name, where: where}
}

// SumOf totals the expression e over the rows, stored in a column called name.
func SumOf(name string, e keel.Expr) Measure {
	return Measure{agg: ir.AggSum, name: name, expr: e}
}

// SumWhere totals the expression e over the rows where holds, stored in a column called name.
func SumWhere(name string, e keel.Expr, where keel.Pred) Measure {
	return Measure{agg: ir.AggSum, name: name, expr: e, where: where}
}

// Histogram counts the rows where holds (every row, for a nil where) by which of the bins bounds
// marks e falls in, one CountWhere per bin named prefix_0, prefix_1, …: bin i holds values from
// bounds[i] up to bounds[i+1], and the last bin every value from the last bound up. Values below the
// first bound are not counted. A quantile is read back from the summed bins with Quantile.
func Histogram(prefix string, e keel.Expr, bounds []float64, where keel.Pred) []Measure {
	if len(bounds) == 0 {
		return []Measure{{name: prefix, err: errors.New("a histogram needs at least one bound")}}
	}
	out := make([]Measure, len(bounds))
	for i, lo := range bounds {
		in := keel.Pred(keel.Raw("? >= ?", e, lo))
		if i+1 < len(bounds) {
			in = keel.And(in, keel.Raw("? < ?", e, bounds[i+1]))
		}
		if where != nil {
			in = keel.And(where, in)
		}
		out[i] = CountWhere(prefix+"_"+strconv.Itoa(i), in)
	}
	return out
}

// Quantile reads the q quantile (0 to 1) from a histogram's summed bin counts and the bounds it was
// declared with, interpolating linearly inside the bin it falls in; the open last bin answers its
// lower bound. It returns 0 when every bin is empty.
func Quantile(counts []int64, bounds []float64, q float64) float64 {
	var total int64
	for _, c := range counts {
		total += c
	}
	if total == 0 || len(counts) == 0 || len(bounds) < len(counts) {
		return 0
	}
	target := q * float64(total)
	var seen float64
	for i, c := range counts {
		if c == 0 {
			continue
		}
		if seen+float64(c) >= target {
			if i+1 >= len(bounds) {
				return bounds[i]
			}
			lo, hi := bounds[i], bounds[i+1]
			return lo + (hi-lo)*(target-seen)/float64(c)
		}
		seen += float64(c)
	}
	return bounds[len(counts)-1]
}

// Measures returns ms, for a declaration that reads as a list.
func Measures(ms ...Measure) []Measure { return ms }

// Option configures a rollup.
type Option func(*options)

// options are a rollup's settings beyond its shape.
type options struct {
	zone        string
	tenantZone  keel.Expr
	where       keel.Pred
	sourceIndex bool
}

// Zone computes the rollup's buckets in the IANA time zone name instead of UTC. A report is
// answered from the rollup only when it runs in the same zone.
func Zone(name string) Option { return func(o *options) { o.zone = name } }

// TenantZone computes each tenant's buckets in that tenant's own time zone: zone is an expression
// over a source row naming an IANA zone, such as a lookup of the row's organization's zone
// (keel.StringOf(keel.Raw("(SELECT o.timezone FROM organizations o WHERE o.id = ?)", t.OrgID))).
// The zone must be a name Postgres knows for every row; when a tenant's zone changes, Rebuild the
// tenant. A tenant-zoned rollup is read directly, never routed to: a report runs in one zone.
func TenantZone(zone keel.Expr) Option { return func(o *options) { o.tenantZone = zone } }

// Where keeps only the source rows where holds: the rollup aggregates them alone. Changes to the
// columns where reads still mark slices, so a row leaving or entering the filter refreshes them.
func Where(holds keel.Pred) Option { return func(o *options) { o.where = holds } }

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
	tenantZone  keel.Expr
	where       keel.Pred
	customs     []customMeasure
	sourceIndex bool
	cols        []column
	table       *schema.Table
}

// customMeasure is a measure over an expression: fn is "count" (rows where holds) or "sum" (of expr,
// where holds when set), stored in the column name.
type customMeasure struct {
	name  string
	fn    string
	expr  keel.Expr
	where keel.Pred
}

// resolvedMeasure is a declared measure on a source column index, -1 for Count. custom is one more
// than the index in customs of a measure over an expression, zero for any other.
type resolvedMeasure struct {
	agg    ir.Agg
	col    int32
	custom int
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
	r := &Rollup{
		name: name, source: table, unit: grain.unit, zoneName: o.zone, sourceIndex: o.sourceIndex,
		tenantZone: o.tenantZone, where: o.where,
	}
	if r.tenantZone != nil {
		r.zoneName = ""
	}
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
	var err error
	if r.tenantZone == nil {
		loc, lerr := time.LoadLocation(r.zoneName)
		if lerr != nil {
			return fmt.Errorf("time zone %q: %w", r.zoneName, lerr)
		}
		r.zone = loc
	} else if _, err = r.inline(r.tenantZone, "s"); err != nil {
		return fmt.Errorf("tenant zone: %w", err)
	}
	if r.where != nil {
		if _, err = r.inline(r.where, "s"); err != nil {
			return fmt.Errorf("where: %w", err)
		}
	}
	if r.grain, err = r.own(grain.col); err != nil {
		return fmt.Errorf("grain: %w", err)
	}
	if err := r.checkGrain(); err != nil {
		return err
	}
	for _, d := range dims {
		idx, err := r.own(d.col)
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
		rm, err := r.checkMeasure(m)
		if err != nil {
			return err
		}
		if !slices.Contains(r.measures, rm) {
			r.measures = append(r.measures, rm)
		}
	}
	return nil
}

// own returns the index of c, checking it is a column of the source table itself.
func (r *Rollup) own(c colRef) (int32, error) {
	if c.err != nil {
		return -1, c.err
	}
	if c.table != r.source {
		return -1, fmt.Errorf("%s.%s is not a column of %s", c.table.Name,
			c.table.Columns[c.index].Name, r.source.Name)
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
func (r *Rollup) checkMeasure(m Measure) (resolvedMeasure, error) {
	if m.err != nil {
		return resolvedMeasure{}, fmt.Errorf("measure %s: %w", m.name, m.err)
	}
	if m.rows {
		return resolvedMeasure{agg: ir.AggCount, col: -1}, nil
	}
	if m.name != "" {
		return r.checkCustom(m)
	}
	idx, err := r.own(m.col)
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

// columnName is what a column a measure names may be called.
var columnName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// checkCustom resolves a measure over an expression, checking its name and that its expressions
// read only the source table's own columns.
func (r *Rollup) checkCustom(m Measure) (resolvedMeasure, error) {
	if !columnName.MatchString(m.name) {
		return resolvedMeasure{}, fmt.Errorf("measure %s: the name must match %s", m.name, columnName)
	}
	cm := customMeasure{name: m.name, fn: "count", expr: m.expr, where: m.where}
	if m.agg == ir.AggSum {
		if m.expr == nil {
			return resolvedMeasure{}, fmt.Errorf("measure %s: a sum needs an expression", m.name)
		}
		cm.fn = "sum"
		if _, err := r.inline(m.expr, "s"); err != nil {
			return resolvedMeasure{}, fmt.Errorf("measure %s: %w", m.name, err)
		}
	} else if m.where == nil {
		return resolvedMeasure{}, fmt.Errorf("measure %s: a count needs a condition; Count counts every row", m.name)
	}
	if m.where != nil {
		if _, err := r.inline(m.where, "s"); err != nil {
			return resolvedMeasure{}, fmt.Errorf("measure %s: %w", m.name, err)
		}
	}
	for _, c := range r.customs {
		if c.name == m.name {
			return resolvedMeasure{}, fmt.Errorf("measure %s is declared twice", m.name)
		}
	}
	r.customs = append(r.customs, cm)
	return resolvedMeasure{agg: m.agg, col: -1, custom: len(r.customs)}, nil
}

// inline renders e over the source table under alias, as SQL with its values written in.
func (r *Rollup) inline(e keel.Expr, alias string) (string, error) {
	sql, _, err := keel.InlineSQL(e, r.source, alias)
	return sql, err
}

// mustInline is inline for an expression Define already rendered once.
func (r *Rollup) mustInline(e keel.Expr, alias string) string {
	sql, err := r.inline(e, alias)
	if err != nil {
		panic(fmt.Sprintf("rollup: %s: an expression that rendered at Define no longer renders: %v", r.name, err))
	}
	return sql
}

// readsOf returns the source columns e reads.
func (r *Rollup) readsOf(e keel.Expr) []int32 {
	if e == nil {
		return nil
	}
	_, cols, err := keel.InlineSQL(e, r.source, "s")
	if err != nil {
		return nil
	}
	return cols
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

// Location returns the time zone buckets are computed in, nil for a rollup bucketed in each
// tenant's own zone (TenantZone).
func (r *Rollup) Location() *time.Location { return r.zone }

// TenantZoned reports whether buckets are computed in each tenant's own zone.
func (r *Rollup) TenantZoned() bool { return r.tenantZone != nil }

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

// Aggregates returns the declared measures over columns, each once, in declaration order; the
// measures over expressions are the rollup table's own columns and route no report.
func (r *Rollup) Aggregates() []Aggregate {
	out := make([]Aggregate, 0, len(r.measures))
	for _, m := range r.measures {
		if m.custom > 0 {
			continue
		}
		a := Aggregate{Agg: m.agg}
		if m.col >= 0 {
			a.Column = &r.source.Columns[m.col]
		}
		out = append(out, a)
	}
	return out
}
