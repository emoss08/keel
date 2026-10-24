package ir

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emoss08/keel/analytics/formula"
)

// MaxDrillColumns caps the columns of a drill-through record list.
const MaxDrillColumns = 14

// FieldTypes gives drill-through the value type of a field path of the report's entity, so the
// range it builds holds values of the field's own type.
type FieldTypes interface {
	FieldType(path string) (formula.Type, bool)
}

type catalogTypes struct {
	cat    Catalog
	entity string
}

func (c catalogTypes) FieldType(path string) (formula.Type, bool) {
	f, err := resolvePath(c.cat, c.entity, path)
	if err != nil {
		return formula.Unknown, false
	}
	return f.Info.Type, true
}

// CatalogTypes returns FieldTypes that resolve paths of an entity through a catalog.
func CatalogTypes(cat Catalog, entity string) FieldTypes {
	return catalogTypes{cat: cat, entity: entity}
}

// DrillValue is the value of one grouping column in the clicked row.
type DrillValue struct {
	// Column is the grouping column id.
	Column string
	// Value is its value in the row; a bucket is the instant it starts.
	Value any
}

// DrillRequest is a click on a report cell.
type DrillRequest struct {
	// Row holds the grouping columns of the clicked row; a subtotal row holds only its own.
	Row []DrillValue
	// Column is the output column clicked, or a pivot cell; empty drills into the whole row.
	Column string
	// Location is the report time zone that buckets were computed in; nil means UTC.
	Location *time.Location
	// Params are the parameter values the report ran with.
	Params map[string]any
	// Columns are the record-list columns, chosen by the caller among the fields the reader may see.
	Columns []Column
	// PivotValues are the values a discovered or parameter pivot produced, for its Other cell.
	PivotValues []string
	// Limit caps the records; zero leaves the runner's limit.
	Limit int
}

// Drill rewrites an aggregate report into the record list behind one of its cells: the report's
// filters, plus each grouping value of the row (a bucket or band inverted into the half-open range
// it covered, in the report's time zone, and a transformed dimension compared through its
// transform), plus the clicked measure's own filter and pivot scope. The report's dimensions
// across relations are appended to the caller's columns. Errors are Errors.
func Drill(def *Definition, req DrillRequest, types FieldTypes) (*Definition, error) {
	v := newValidator(def, nil, Limits{})
	v.run()
	if len(v.errs) > 0 {
		return nil, v.errs
	}
	d := &driller{v: v, def: def, req: req, types: types, loc: req.Location}
	if d.loc == nil {
		d.loc = time.UTC
	}
	conditions := []Filter{}
	if !def.Filters.Empty() {
		conditions = append(conditions, *def.Filters.Clone())
	}
	for i, rv := range req.Row {
		conditions = append(conditions, d.rowValue(rv, ptr("/row", i))...)
	}
	conditions = append(conditions, d.cellScope()...)
	columns := d.columns()
	if len(d.errs) > 0 {
		return nil, d.errs
	}
	out := &Definition{Version: Version, Entity: def.Entity, Columns: columns, Filters: And(conditions...), Parameters: def.Parameters}
	if req.Limit > 0 {
		out.Limit = N(req.Limit)
	}
	return out, nil
}

type driller struct {
	collector
	v     *validator
	def   *Definition
	req   DrillRequest
	types FieldTypes
	loc   *time.Location
}

func (d *driller) fieldType(path string) formula.Type {
	if d.types == nil {
		return formula.Unknown
	}
	t, _ := d.types.FieldType(path)
	return t
}

func (d *driller) rowValue(rv DrillValue, at string) []Filter {
	c, ok := d.def.ColumnByID(rv.Column)
	if !ok || d.v.levelOf(rv.Column) > formula.Row {
		d.add(CodeInvalid, at, "%q is not a grouping column of this report", rv.Column)
		return nil
	}
	if c.Calc != nil {
		return []Filter{equals(Filter{Calc: c.Calc, Transform: c.Transform}, rv.Value)}
	}
	dim := c.Dimension
	switch {
	case rv.Value == nil:
		return []Filter{{Field: dim.Field, Op: OpIsNull}}
	case !dim.Bucket.IsZero():
		return d.bucketRange(dim, rv.Value, at)
	case dim.Band != nil:
		return d.bandRange(dim, rv.Value, at)
	}
	return []Filter{equals(Filter{Field: dim.Field, Transform: c.Transform}, rv.Value)}
}

func equals(f Filter, value any) Filter {
	if value == nil {
		f.Op = OpIsNull
		return f
	}
	f.Op, f.Value = OpEq, value
	return f
}

func (d *driller) bucketRange(dim *Dimension, value any, at string) []Filter {
	unit := dim.Bucket.Unit
	if dim.Bucket.Param != "" {
		s, _ := d.req.Params[dim.Bucket.Param].(string)
		unit = Bucket(s)
		if p, ok := d.def.Parameter(dim.Bucket.Param); ok && unit == "" {
			s, _ = p.Default.(string)
			unit = Bucket(s)
		}
	}
	if !isOneOf(unit, Buckets()) {
		d.add(CodeRequired, at, "the bucket parameter %q has no value", dim.Bucket.Param)
		return nil
	}
	start, ok := instant(value)
	if !ok {
		d.add(CodeType, at, "expected the instant the bucket starts, got %v", value)
		return nil
	}
	start = start.In(d.loc)
	end := addBucket(start, unit)
	t := d.fieldType(dim.Field)
	return []Filter{
		{Field: dim.Field, Op: OpGte, Value: timeValue(start, t)},
		{Field: dim.Field, Op: OpLt, Value: timeValue(end, t)},
	}
}

func instant(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, true
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t, true
		}
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
		return time.Time{}, false
	default:
		n, ok := asInt(v)
		return time.Unix(n, 0), ok
	}
}

func addBucket(t time.Time, b Bucket) time.Time {
	switch b {
	case BucketHour:
		return t.Add(time.Hour)
	case BucketDay:
		return t.AddDate(0, 0, 1)
	case BucketWeek:
		return t.AddDate(0, 0, 7)
	case BucketMonth:
		return t.AddDate(0, 1, 0)
	case BucketQuarter:
		return t.AddDate(0, 3, 0)
	default:
		return t.AddDate(1, 0, 0)
	}
}

func timeValue(t time.Time, typ formula.Type) any {
	switch typ {
	case formula.Epoch:
		return t.Unix()
	case formula.Date:
		return t.Format(time.DateOnly)
	default:
		return t.Format(time.RFC3339)
	}
}

func (d *driller) bandRange(dim *Dimension, value any, at string) []Filter {
	x, ok := asFloat(value)
	if !ok {
		d.add(CodeType, at, "expected a number for a banded column, got %v", value)
		return nil
	}
	b := dim.Band
	lower, upper, bounded := b.Bounds(x)
	t := d.fieldType(dim.Field)
	var out []Filter
	if !b.lowest(x) {
		out = append(out, Filter{Field: dim.Field, Op: OpGte, Value: number(lower, t)})
	}
	if bounded {
		out = append(out, Filter{Field: dim.Field, Op: OpLt, Value: number(upper, t)})
	}
	if len(out) == 0 {
		return []Filter{{Field: dim.Field, Op: OpIsNotNull}}
	}
	return out
}

func number(x float64, t formula.Type) any {
	if t == formula.Int {
		return int64(x)
	}
	return json.Number(strconv.FormatFloat(x, 'f', -1, 64))
}

// Bounds returns the range a grouped lower bound stands for; the last range of explicit edges is
// open-ended, reported by bounded false.
func (b *Band) Bounds(value float64) (lower, upper float64, bounded bool) {
	if b.Width > 0 {
		return value, value + b.Width, true
	}
	index := 0
	for i, e := range b.Edges {
		if value >= e {
			index = i
		}
	}
	if index == len(b.Edges)-1 {
		return b.Edges[index], 0, false
	}
	return b.Edges[index], b.Edges[index+1], true
}

func (b *Band) lowest(value float64) bool {
	return b.Width == 0 && len(b.Edges) > 0 && value <= b.Edges[0]
}

func (d *driller) cellScope() []Filter {
	if d.req.Column == "" {
		return nil
	}
	base, value, cell := strings.Cut(d.req.Column, PivotSeparator)
	c, ok := d.def.ColumnByID(base)
	if !ok {
		d.add(CodeUnknownColumn, "/column", "%q is not a column of this report", d.req.Column)
		return nil
	}
	var out []Filter
	if m := d.measureOf(c); m != nil && !m.Filter.Empty() {
		out = append(out, *m.Filter.Clone())
	}
	if cell && d.def.Pivot != nil {
		out = append(out, d.pivotScope(value))
	}
	return out
}

func (d *driller) measureOf(c *Column) *Measure {
	if c.Window != nil {
		if of, ok := d.def.ColumnByID(c.Window.Of); ok {
			return of.Measure
		}
	}
	return c.Measure
}

func (d *driller) pivotScope(value string) Filter {
	p := d.def.Pivot
	t := d.fieldType(p.Field)
	if value != PivotOther {
		return Filter{Field: p.Field, Op: OpEq, Value: pivotValue(value, t)}
	}
	listed := p.Values.List
	if listed == nil {
		listed = d.req.PivotValues
	}
	values := make([]any, len(listed))
	for i, x := range listed {
		values[i] = pivotValue(x, t)
	}
	if len(values) == 0 {
		return Filter{Field: p.Field, Op: OpIsNull}
	}
	return *Or(Filter{Field: p.Field, Op: OpIsNull}, Filter{Field: p.Field, Op: OpNotIn, Value: values})
}

func pivotValue(s string, t formula.Type) any {
	switch {
	case t.Numeric():
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return json.Number(s)
		}
	case t == formula.Bool:
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	}
	return s
}

func (d *driller) columns() []Column {
	out := append([]Column{}, d.req.Columns...)
	seen, ids := map[string]bool{}, map[string]bool{}
	for _, c := range out {
		ids[c.ID] = true
		if c.Dimension != nil {
			seen[c.Dimension.Field] = true
		}
	}
	for _, c := range d.def.Columns {
		if len(out) >= MaxDrillColumns {
			break
		}
		if c.Dimension == nil || !strings.Contains(c.Dimension.Field, ".") || seen[c.Dimension.Field] {
			continue
		}
		seen[c.Dimension.Field] = true
		id := fmt.Sprintf("d%d", len(out))
		for n := 1; ids[id]; n++ {
			id = fmt.Sprintf("d%d_%d", len(out), n)
		}
		ids[id] = true
		out = append(out, Column{ID: id, Label: c.Label, Dimension: &Dimension{Field: c.Dimension.Field}})
	}
	if len(out) == 0 {
		d.add(CodeRequired, "/columns", "the record list needs at least one column the reader may see")
	}
	return out
}
