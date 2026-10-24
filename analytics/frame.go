package analytics

import (
	"strings"

	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
	"github.com/emoss08/keel/schema"
)

// frame builds expressions over one dynamic query whose base is the report's entity: the grouped
// statement, a dynamic pivot's discovery statement, or the base of a fan-out distinct count. Each
// field, column and pre-aggregation is built once per frame and reused, so an expression written
// in SELECT, GROUP BY, HAVING and ORDER BY binds its values once.
type frame struct {
	c       *compiler
	q       *keel.DynQuery
	fields  map[string]keel.DynExpr
	cols    map[string]keel.DynExpr
	preaggs map[string]*keel.DynPreAgg
	inner   map[string]keel.DynExpr
}

// newFrame returns a frame over q.
func newFrame(c *compiler, q *keel.DynQuery) *frame {
	return &frame{c: c, q: q, fields: map[string]keel.DynExpr{}, cols: map[string]keel.DynExpr{},
		preaggs: map[string]*keel.DynPreAgg{}, inner: map[string]keel.DynExpr{}}
}

// fieldPath is a field path resolved against the catalog: the edges, the field, and where the
// path crosses to-many edges.
type fieldPath struct {
	path  string
	via   Path
	field *Field
	// many is the index in via of the last to-many edge, or -1.
	many int
}

// resolvePath resolves a field path of the report's entity.
func (c *compiler) resolvePath(path string) (fieldPath, error) {
	via, f, ok := c.cat.Resolve(c.def.Entity, path)
	if !ok {
		return fieldPath{}, fail(CodeInvalid, "", "unknown field %s", path)
	}
	fp := fieldPath{path: path, via: via, field: f, many: -1}
	for i, e := range via {
		if e.ToMany {
			fp.many = i
		}
	}
	c.notePath(fp)
	return fp, nil
}

// notePath records the tables a path reads, the joins it makes and the to-many paths it
// crosses.
func (c *compiler) notePath(fp fieldPath) {
	toMany := false
	for i, e := range fp.via {
		c.tables[e.Target.Table] = true
		key := fp.via[:i+1].String()
		switch {
		case e.ToMany:
			toMany = true
			c.manyKeys[key] = true
		case !toMany:
			c.joinKeys[key] = true
		}
		rel := e.relation()
		if rel.Kind != schema.Pick {
			continue
		}
		t := e.From.Table
		for _, ri := range rel.Path {
			t = t.Relations[ri].Target
			c.tables[t] = true
		}
	}
}

// relNames returns the schema relation names of edges.
func relNames(via Path) []string {
	out := make([]string, len(via))
	for i, e := range via {
		out[i] = e.relation().Name
	}
	return out
}

// colName returns the SQL name of a field's column.
func colName(f *Field) string { return f.column().Name }

// field returns a field's value per row: its column through the to-one edges of its path, joined
// on demand. A path across a to-many edge has no single value per row.
func (f *frame) field(path string) (keel.DynExpr, fieldPath, error) {
	fp, err := f.c.resolvePath(path)
	if err != nil {
		return keel.DynExpr{}, fp, err
	}
	if fp.many >= 0 {
		return keel.DynExpr{}, fp, fail(CodeNotAllowed, "", "%s crosses the to-many relation %s, so it has no single value per row; aggregate it in a measure",
			path, fp.via[fp.many].Name)
	}
	if e, ok := f.fields[path]; ok {
		return e, fp, nil
	}
	if f.c.rollup != nil {
		e, err := f.c.rollupField(f.q, path)
		if err != nil {
			return keel.DynExpr{}, fp, err
		}
		f.fields[path] = e
		return e, fp, nil
	}
	e := f.q.Path(append(relNames(fp.via), colName(fp.field))...)
	if err := e.Err(); err != nil {
		return keel.DynExpr{}, fp, err
	}
	f.fields[path] = e
	return e, fp, nil
}

// display returns a field's value as reports show it: an enum stored as an integer is shown as
// the text of its value.
func display(e keel.DynExpr, fp fieldPath) keel.DynExpr {
	if fp.field != nil && fp.field.numericEnum() {
		return e.Cast(keel.TypeString)
	}
	return e
}

// manyKey identifies the pre-aggregation of a to-many path: its edges up to the last to-many one.
func manyKey(fp fieldPath) string {
	names := make([]string, fp.many+1)
	for i, e := range fp.via[:fp.many+1] {
		names[i] = e.Name
	}
	return strings.Join(names, ".")
}

// preagg returns the pre-aggregation of a to-many path, made on first use with the compiler's
// strategy.
func (f *frame) preagg(fp fieldPath) *keel.DynPreAgg {
	key := manyKey(fp)
	if p, ok := f.preaggs[key]; ok {
		return p
	}
	p := f.q.Rel(relNames(fp.via[:fp.many+1])...).PreAggregate(f.c.strategy())
	if f.c.opts.Strategy == StrategyCTEAll {
		p.AllParents()
	}
	f.preaggs[key] = p
	f.c.notePreagg(key)
	return p
}

// childCol returns a field of a to-many path's rows inside its pre-aggregation: the column
// through the to-one edges after the last to-many one.
func (f *frame) childCol(fp fieldPath) keel.DynExpr {
	rel := f.q.Rel(relNames(fp.via[:fp.many+1])...)
	return rel.Col(append(relNames(fp.via[fp.many+1:]), colName(fp.field))...)
}

// innerMeasure adds an aggregate to a pre-aggregation once and returns its per-parent column.
func (f *frame) innerMeasure(fp fieldPath, key string, agg keel.DynExpr) keel.DynExpr {
	full := manyKey(fp) + "\x00" + key
	if e, ok := f.inner[full]; ok {
		return e
	}
	e := f.preagg(fp).Measure(agg)
	f.inner[full] = e
	return e
}

// aggregate applies a measure aggregation to a per-row value, with a filter, and casts a sum of
// integers back to an integer. t is the aggregate's type: an average of instants averages their
// seconds and returns the instant at the mean.
func aggregate(agg ir.Agg, x keel.DynExpr, filter keel.Pred, t formula.Type) keel.DynExpr {
	if agg == ir.AggAvg && t.Temporal() {
		e := seconds(x, t).Avg()
		if filter != nil {
			e = e.Filter(filter)
		}
		return instantAt(e, t)
	}
	var e keel.DynExpr
	switch agg {
	case ir.AggCount:
		e = x.Count()
	case ir.AggCountDistinct:
		e = x.CountDistinct()
	case ir.AggSum:
		e = x.Sum()
	case ir.AggAvg:
		e = x.Avg()
	case ir.AggMin:
		e = x.Min()
	default:
		e = x.Max()
	}
	if filter != nil {
		e = e.Filter(filter)
	}
	if agg == ir.AggSum && t == formula.Int {
		e = e.Cast(keel.TypeInt64)
	}
	return e
}

// countRows is count(*) with a filter.
func countRows(filter keel.Pred) keel.DynExpr {
	e := keel.DynCount()
	if filter != nil {
		e = e.Filter(filter)
	}
	return e
}

// manyMeasure aggregates x, a value of a to-many path's rows, across the path: each parent's related rows are
// aggregated once in a pre-aggregation, and the per-parent values are aggregated again per group,
// so the parent's other measures are not fanned out. An average is a sum over a count.
func (f *frame) manyMeasure(agg ir.Agg, fp fieldPath, x keel.DynExpr, inner keel.Pred, innerKey string,
	outer keel.Pred, t formula.Type,
) keel.DynExpr {
	key := string(agg) + "\x00" + fp.path + "\x00" + innerKey
	switch agg {
	case ir.AggSum:
		return aggregate(ir.AggSum, f.innerMeasure(fp, key, aggregate(ir.AggSum, x, inner, formula.Decimal)), outer, t)
	case ir.AggCount, ir.AggCountDistinct:
		per := f.innerMeasure(fp, key, aggregate(agg, x, inner, formula.Int))
		return coalesceZero(aggregate(ir.AggSum, per, outer, formula.Int))
	case ir.AggMin, ir.AggMax:
		return aggregate(agg, f.innerMeasure(fp, key, aggregate(agg, x, inner, t)), outer, t)
	}
	sum := f.innerMeasure(fp, "sum\x00"+key, aggregate(ir.AggSum, seconds(x, t), inner, formula.Decimal))
	n := f.innerMeasure(fp, "count\x00"+key, aggregate(ir.AggCount, x, inner, formula.Int))
	return mean(aggregate(ir.AggSum, sum, outer, formula.Decimal), aggregate(ir.AggSum, n, outer, formula.Decimal), t)
}

// mean divides a total by a count of type t's values, nothing over no values: floating point for
// floats, the instant at the mean of instants' seconds, and an exact decimal otherwise.
func mean(total, count keel.DynExpr, t formula.Type) keel.DynExpr {
	if t == formula.Float {
		return total.Cast(keel.TypeFloat64).Div(count.NullIf(0))
	}
	m := total.Cast(keel.TypeNumeric).Div(count.NullIf(0))
	if t.Temporal() {
		return instantAt(m, t)
	}
	return m
}

// seconds returns a value of type t as the number a mean of it averages: an instant's seconds
// since 1970 (a date's days), anything else as it is.
func seconds(x keel.DynExpr, t formula.Type) keel.DynExpr {
	switch t {
	case formula.Timestamp:
		return keel.DynOf(keel.Raw("extract(epoch from ?)", x))
	case formula.Date:
		return keel.DynOf(keel.Raw("(? - DATE '1970-01-01')", x))
	}
	return x
}

// instantAt returns the instant of type t a mean of seconds denotes: epoch seconds rounded to the
// second, a timestamp, or a date rounded to the day.
func instantAt(m keel.DynExpr, t formula.Type) keel.DynExpr {
	switch t {
	case formula.Timestamp:
		return keel.DynOf(keel.Raw("to_timestamp(?)", m))
	case formula.Date:
		return keel.DynOf(keel.Raw("(DATE '1970-01-01' + CAST(round(?) AS integer))", m))
	}
	return keel.DynOf(keel.Raw("CAST(round(?) AS bigint)", m))
}

// coalesceZero is coalesce(e, 0): a group without related rows counts zero.
func coalesceZero(e keel.DynExpr) keel.DynExpr {
	return e.Coalesce(keel.DynLit(int64(0)).Cast(keel.TypeInt64))
}

// distinctByParent reports whether a distinct count across a to-many path can be summed from the
// per-parent counts: it counts the primary key of the last to-many table, reached without a
// many-to-many edge or a to-one edge after it, so no row belongs to two parents.
func distinctByParent(fp fieldPath) bool {
	if fp.many < 0 || fp.many != len(fp.via)-1 {
		return false
	}
	for _, e := range fp.via {
		if e.relation().Kind == schema.ManyToMany {
			return false
		}
	}
	t := fp.field.Entity.Table
	keys := 0
	for _, pk := range t.PK {
		if t.Columns[pk].Flags.Has(schema.FlagTenant) {
			continue
		}
		keys++
		if pk != fp.field.Column {
			return false
		}
	}
	return keys == 1
}

// strategy returns how this compilation pre-aggregates to-many paths.
func (c *compiler) strategy() keel.PreAggStrategy {
	switch c.opts.Strategy {
	case StrategyCTE, StrategyCTEAll:
		return keel.PreAggCTE
	case StrategyLateral:
		return keel.PreAggLateral
	}
	if c.fewParents() {
		return keel.PreAggLateral
	}
	return keel.PreAggCTE
}

// fewParents reports whether the report's filters pin the entity's key to at most
// LateralParents values: a top-level condition (or one inside the top-level and group) that
// compares the key with eq, or with in and a short enough list.
func (c *compiler) fewParents() bool {
	t := c.base.Table
	var key *Field
	for _, f := range c.base.Fields {
		if t.Columns[f.Column].Flags.Has(schema.FlagPK) && !t.Columns[f.Column].Flags.Has(schema.FlagTenant) {
			key = f
		}
	}
	if key == nil || c.def.Filters == nil {
		return false
	}
	conds := []ir.Filter{*c.def.Filters}
	if c.def.Filters.And != nil {
		conds = c.def.Filters.And
	}
	for _, f := range conds {
		if f.IsGroup() || f.Field != key.Key || f.Transform != nil {
			continue
		}
		v := f.Value
		if f.Param != "" {
			v = c.params[f.Param]
		}
		switch f.Op {
		case ir.OpEq:
			return v != nil
		case ir.OpIn:
			list, ok := listOf(v)
			return ok && len(list) <= c.opts.LateralParents
		}
	}
	return false
}

// notePreagg counts a pre-aggregation once per compilation.
func (c *compiler) notePreagg(key string) { c.preaggKeys[key] = true }
