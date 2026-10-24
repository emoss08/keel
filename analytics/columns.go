package analytics

import (
	"github.com/emoss08/keel"
	"github.com/emoss08/keel/analytics/formula"
	"github.com/emoss08/keel/analytics/ir"
)

// column returns a column's value in the frame for a pivot cell ("" for none), built once.
func (f *frame) column(p *colPlan, cell string) (keel.DynExpr, error) {
	key := label(p.def.ID, cell)
	if e, ok := f.cols[key]; ok {
		return e, nil
	}
	var e keel.DynExpr
	var err error
	switch {
	case p.def.Dimension != nil:
		e, err = f.dimension(p)
	case p.def.Measure != nil:
		e, err = f.measure(p, cell)
	case p.def.Calc != nil:
		e, err = f.c.render(f.c.res.Calcs[p.def.ID], f.env(cell))
	default:
		err = fail(CodeNotAllowed, "", "%s is computed over the grouped result", p.def.ID)
	}
	if err != nil {
		return keel.DynExpr{}, err
	}
	if p.def.Dimension == nil && p.def.Transform != nil {
		e = transform(e, p.def.Transform, f.c.preTransform(p))
	}
	f.cols[key] = e
	return e, nil
}

// preTransform returns a column's type before its transform.
func (c *compiler) preTransform(p *colPlan) formula.Type {
	switch {
	case p.def.Measure != nil:
		in := formula.Unknown
		if p.def.Measure.Field != "" {
			if _, f, ok := c.cat.Resolve(c.def.Entity, p.def.Measure.Field); ok {
				in = f.Type
			}
		}
		t, err := formula.CallType(string(p.def.Measure.Agg), in)
		if err != nil || p.def.Measure.Field == "" {
			return formula.Int
		}
		return t
	case p.def.Calc != nil:
		if t, ok := c.res.Calcs[p.def.ID]; ok {
			return t.Type
		}
	case p.def.Dimension != nil:
		if _, f, ok := c.cat.Resolve(c.def.Entity, p.def.Dimension.Field); ok {
			return f.Type
		}
	}
	return formula.Unknown
}

// dimension returns a dimension's value: the field, bucketed in the report's time zone or banded,
// then transformed.
func (f *frame) dimension(p *colPlan) (keel.DynExpr, error) {
	d := p.def.Dimension
	e, fp, err := f.field(d.Field)
	if err != nil {
		return keel.DynExpr{}, err
	}
	t := fp.field.Type
	switch {
	case !d.Bucket.IsZero():
		unit, err := f.c.bucketUnit(d.Bucket)
		if err != nil {
			return keel.DynExpr{}, err
		}
		e = f.c.bucket(e, t, unit)
	case d.Band != nil:
		e = band(e, d.Band, t)
	default:
		e = display(e, fp)
	}
	if p.def.Transform != nil {
		e = transform(e, p.def.Transform, t)
	}
	return e, nil
}

// bucketUnit returns a bucket's unit: given, or chosen by a parameter.
func (c *compiler) bucketUnit(g ir.Grain) (ir.Bucket, error) {
	if g.Param == "" {
		return g.Unit, nil
	}
	s, _ := c.params[g.Param].(string)
	for _, b := range ir.Buckets() {
		if string(b) == s {
			return b, nil
		}
	}
	return "", fail(CodeParameter, "", "the bucket parameter %q has no bucket value", g.Param)
}

// bucket truncates an instant to the start of its period in the report's time zone and returns
// that instant in the field's own type: a timestamp, epoch seconds, or for a date the date the
// period starts on.
func (c *compiler) bucket(e keel.DynExpr, t formula.Type, unit ir.Bucket) keel.DynExpr {
	u := lit(string(unit), formula.String)
	switch t {
	case formula.Epoch:
		return keel.DynOf(keel.Raw("CAST(extract(epoch from date_trunc(?, to_timestamp(?), ?)) AS bigint)", u, e, c.tz))
	case formula.Date:
		return keel.DynOf(keel.Raw("CAST(date_trunc(?, CAST(? AS timestamp)) AS date)", u, e))
	}
	return keel.DynOf(keel.Raw("date_trunc(?, ?, ?)", u, e, c.tz))
}

// band returns the lower bound of a number's band, in the field's type: floor(x / width) ×
// width, or the last boundary not above x (the first for values below it); NULL stays NULL.
func band(e keel.DynExpr, b *ir.Band, t formula.Type) keel.DynExpr {
	if b.Width > 0 {
		w := lit(floatDecimal(b.Width), formula.Decimal)
		return castTo(keel.DynOf(keel.Raw("(floor(CAST(? AS numeric) / ?) * ?)", e, w, w)), t)
	}
	cb := keel.DynCase()
	for i := 1; i < len(b.Edges); i++ {
		cb = cb.When(e.Lt(edgeLit(b.Edges[i], t)), edgeLit(b.Edges[i-1], t))
	}
	last := edgeLit(b.Edges[len(b.Edges)-1], t)
	return cb.When(e.Gte(last), last).End()
}

// edgeLit binds a band boundary in the field's type.
func edgeLit(x float64, t formula.Type) keel.DynExpr {
	switch t {
	case formula.Int:
		return lit(int64(x), formula.Int)
	case formula.Float:
		return lit(x, formula.Float)
	}
	return lit(floatDecimal(x), formula.Decimal)
}

// floatDecimal writes a float as an exact decimal.
func floatDecimal(x float64) keel.Decimal {
	d, _ := toDecimal(x)
	return d
}

// castTo casts a numeric value to the column type t.
func castTo(e keel.DynExpr, t formula.Type) keel.DynExpr {
	switch t {
	case formula.Int:
		return e.Cast(keel.TypeInt64)
	case formula.Float:
		return e.Cast(keel.TypeFloat64)
	}
	return e
}

// measure returns a measure's aggregate for a pivot cell: over the rows of the entity and its
// to-one relations, or pre-aggregated per parent across a to-many path.
func (f *frame) measure(p *colPlan, cell string) (keel.DynExpr, error) {
	m := p.def.Measure
	pivot, err := f.pivotPred(cell)
	if err != nil {
		return keel.DynExpr{}, err
	}
	if f.c.rollup != nil {
		return f.c.rollupMeasure(f, p, pivot)
	}
	at := "/columns/" + itoa(f.c.indexOf(p)) + "/measure/filter"
	if m.Field == "" {
		filter, err := f.c.filterPred(m.Filter, at, f.whereEnv())
		if err != nil {
			return keel.DynExpr{}, err
		}
		return countRows(andPred(filter, pivot)), nil
	}
	fp, err := f.c.resolvePath(m.Field)
	if err != nil {
		return keel.DynExpr{}, err
	}
	t := f.c.preTransform(p)
	if fp.many < 0 {
		x, _, err := f.field(m.Field)
		if err != nil {
			return keel.DynExpr{}, err
		}
		filter, err := f.c.filterPred(m.Filter, at, f.whereEnv())
		if err != nil {
			return keel.DynExpr{}, err
		}
		return aggregate(m.Agg, x, andPred(filter, pivot), t), nil
	}
	inner, err := f.c.filterPred(m.Filter, at, f.childEnv())
	if err != nil {
		return keel.DynExpr{}, err
	}
	return f.manyMeasure(m.Agg, fp, f.childCol(fp), inner, filterKey(m.Filter), pivot, t), nil
}

// indexOf returns a column's index in the definition.
func (c *compiler) indexOf(p *colPlan) int {
	for i, q := range c.cols {
		if q == p {
			return i
		}
	}
	return -1
}

// andPred joins two predicates, either of which may be nil.
func andPred(a, b keel.Pred) keel.Pred {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return keel.And(a, b)
}

// pivotPred returns the condition of a pivot cell: the pivot field equal to the cell's value, or
// for Other none of the values (nulls included); nil for no cell.
func (f *frame) pivotPred(cell string) (keel.Pred, error) {
	if cell == "" || f.c.pivot == nil {
		return nil, nil
	}
	e, fp, err := f.field(f.c.pivot.def.Field)
	if err != nil {
		return nil, err
	}
	s := subject{e: e, t: fp.field.Type, field: fp.field}
	if f.c.rollup != nil {
		s.field = nil
	}
	if cell != ir.PivotOther {
		return f.c.compare(s, ir.OpEq, cell, "/pivot/values")
	}
	if len(f.c.pivot.values) == 0 {
		return nil, nil
	}
	values := make([]any, len(f.c.pivot.values))
	for i, v := range f.c.pivot.values {
		values[i] = v
	}
	return f.c.compare(s, ir.OpNotIn, values, "/pivot/values")
}

// aggregateCall renders an aggregate call of a formula over fields for a pivot cell: over the
// entity's rows when its fields are the entity's or its to-one relations', pre-aggregated when
// they are all on one to-many path.
func (f *frame) aggregateCall(t *formula.Typed, cell string) (keel.DynExpr, error) {
	pivot, err := f.pivotPred(cell)
	if err != nil {
		return keel.DynExpr{}, err
	}
	agg := ir.Agg(t.Func.Name)
	if len(t.Args) == 0 {
		return countRows(pivot), nil
	}
	arg := t.Args[0]
	fp, many, err := f.c.aggregateLevel(arg)
	if err != nil {
		return keel.DynExpr{}, err
	}
	if f.c.rollup != nil && many {
		return keel.DynExpr{}, errRollupMiss
	}
	if !many {
		x, err := f.c.render(arg, f.env(cell))
		if err != nil {
			return keel.DynExpr{}, err
		}
		return f.c.castResult(t, aggregate(agg, x, pivot, t.Type))
	}
	if agg == ir.AggCountDistinct && (arg.Node.Kind != formula.KindField || !distinctByParent(fp)) {
		return keel.DynExpr{}, fail(CodeNotAllowed, "", "count_distinct across the to-many relation %s counts a field in a measure column, not inside a calculation",
			fp.via[fp.many].Name)
	}
	x, err := f.c.render(arg, f.childFormulaEnv())
	if err != nil {
		return keel.DynExpr{}, err
	}
	key := formula.Format(arg.Node)
	return f.c.castResult(t, f.manyMeasure(agg, fp, x, nil, key, pivot, t.Type))
}

// aggregateLevel returns where an aggregate's argument reads: the entity's rows, or one to-many
// path whose fields it all reads (returned).
func (c *compiler) aggregateLevel(arg *formula.Typed) (fieldPath, bool, error) {
	var first fieldPath
	many := false
	for i, path := range arg.Node.References(formula.KindField) {
		fp, err := c.resolvePath(path)
		if err != nil {
			return fp, false, err
		}
		isMany := fp.many >= 0
		switch {
		case i == 0:
			first, many = fp, isMany
		case isMany != many || isMany && manyKey(fp) != manyKey(first) || isMany && !sameEdges(fp, first):
			return fp, false, fail(CodeNotAllowed, "", "an aggregate reads either the report's rows or one to-many relation's rows, and this one mixes %s with %s",
				first.path, fp.path)
		}
	}
	return first, many, nil
}

// sameEdges reports whether two paths cross the same edges.
func sameEdges(a, b fieldPath) bool { return a.via.String() == b.via.String() }

// env is the formula environment of the frame for a pivot cell.
func (f *frame) env(cell string) formulaEnv { return frameEnv{f: f, cell: cell} }

// frameEnv renders formulas over the frame's statement.
type frameEnv struct {
	f    *frame
	cell string
}

// column returns a referenced column in the same cell when it is pivoted.
func (e frameEnv) column(id string) (keel.DynExpr, error) {
	p := e.f.c.byID[id]
	if p.outer {
		return keel.DynExpr{}, fail(CodeNotAllowed, "", "%s is computed over the grouped result", id)
	}
	cell := ""
	if p.pivoted {
		cell = e.cell
	}
	return e.f.column(p, cell)
}

// field returns a field's value as reports show it.
func (e frameEnv) field(path string) (keel.DynExpr, error) {
	x, fp, err := e.f.field(path)
	if err != nil {
		return keel.DynExpr{}, err
	}
	return display(x, fp), nil
}

// aggregate renders an aggregate call over fields.
func (e frameEnv) aggregate(t *formula.Typed) (keel.DynExpr, error) {
	return e.f.aggregateCall(t, e.cell)
}

// childFormulaEnv renders an aggregate's argument over a to-many path's rows, inside its
// pre-aggregation.
func (f *frame) childFormulaEnv() formulaEnv { return childEnv{f: f} }

// childEnv resolves fields to a to-many path's columns inside its pre-aggregation.
type childEnv struct{ f *frame }

// column is not available inside a pre-aggregation.
func (e childEnv) column(id string) (keel.DynExpr, error) {
	return keel.DynExpr{}, fail(CodeNotAllowed, "", "an aggregate's argument cannot reference the column %s", id)
}

// field returns a field of the to-many path's rows.
func (e childEnv) field(path string) (keel.DynExpr, error) {
	fp, err := e.f.c.resolvePath(path)
	if err != nil {
		return keel.DynExpr{}, err
	}
	return display(e.f.childCol(fp), fp), nil
}

// aggregate is not available inside an aggregate.
func (e childEnv) aggregate(*formula.Typed) (keel.DynExpr, error) {
	return keel.DynExpr{}, fail(CodeNotAllowed, "", "an aggregate cannot contain another aggregate")
}

// whereEnv resolves the subjects of the report's filters and of measure filters over the
// entity's rows: fields across a to-many path are tested with EXISTS over the path.
func (f *frame) whereEnv() condEnv {
	return condEnv{scope: scopeWhere,
		field: func(path string) (subject, func(keel.Pred) keel.Pred, error) {
			fp, err := f.c.resolvePath(path)
			if err != nil {
				return subject{}, nil, err
			}
			if fp.many >= 0 {
				if f.c.rollup != nil {
					return subject{}, nil, errRollupMiss
				}
				rel := f.q.Rel(relNames(fp.via[:fp.many+1])...)
				x := rel.Col(append(relNames(fp.via[fp.many+1:]), colName(fp.field))...)
				return subject{e: x, t: fp.field.Type, field: fp.field},
					func(p keel.Pred) keel.Pred { return rel.Any(p) }, nil
			}
			x, _, err := f.field(path)
			if err != nil {
				return subject{}, nil, err
			}
			s := subject{e: x, t: fp.field.Type, field: fp.field}
			if f.c.rollup != nil {
				s.field = nil
			}
			return s, identity, nil
		},
		column: func(id string) (keel.DynExpr, error) { return f.env("").column(id) },
		formula: func(t *formula.Typed) (keel.DynExpr, error) {
			return f.c.render(t, f.env(""))
		},
	}
}

// childEnv resolves the subjects of a to-many measure's filter to its path's columns.
func (f *frame) childEnv() condEnv {
	return condEnv{scope: scopeMeasure,
		field: func(path string) (subject, func(keel.Pred) keel.Pred, error) {
			fp, err := f.c.resolvePath(path)
			if err != nil {
				return subject{}, nil, err
			}
			return subject{e: f.childCol(fp), t: fp.field.Type, field: fp.field}, identity, nil
		},
		column: func(id string) (keel.DynExpr, error) { return f.env("").column(id) },
		formula: func(t *formula.Typed) (keel.DynExpr, error) {
			return f.c.render(t, f.childFormulaEnv())
		},
	}
}

// identity leaves a predicate as it is.
func identity(p keel.Pred) keel.Pred { return p }

// outerFrame builds the columns of the statement over the grouped result.
type outerFrame struct {
	c    *compiler
	q    *keel.DynQuery
	fan  *keel.DynCTE
	cols map[string]keel.DynExpr
}

// column returns a column of the grouped result for a pivot cell.
func (o *outerFrame) column(p *colPlan, cell string) (keel.DynExpr, error) {
	key := label(p.def.ID, cell)
	if e, ok := o.cols[key]; ok {
		return e, nil
	}
	var e keel.DynExpr
	var err error
	switch {
	case !p.outer:
		e = o.q.Path(key)
	case p.fanout:
		e = coalesceZero(o.fan.Col(key))
	case p.def.Window != nil:
		e, err = o.window(p)
	default:
		e, err = o.c.render(o.c.res.Calcs[p.def.ID], o.env(cell))
		if err == nil && p.def.Transform != nil {
			e = transform(e, p.def.Transform, o.c.preTransform(p))
		}
	}
	if err != nil {
		return keel.DynExpr{}, err
	}
	o.cols[key] = e
	return e, nil
}

// env is the formula environment of the outer statement for a pivot cell.
func (o *outerFrame) env(cell string) formulaEnv { return outerEnv{o: o, cell: cell} }

// outerEnv renders calculations over the grouped result.
type outerEnv struct {
	o    *outerFrame
	cell string
}

// column returns a referenced column in the same cell when it is pivoted.
func (e outerEnv) column(id string) (keel.DynExpr, error) {
	p := e.o.c.byID[id]
	cell := ""
	if p.pivoted {
		cell = e.cell
	}
	return e.o.column(p, cell)
}

// field is not available over the grouped result: validation keeps per-row fields out of
// calculations over groups.
func (e outerEnv) field(path string) (keel.DynExpr, error) {
	return keel.DynExpr{}, fail(CodeNotAllowed, "", "{%s} is a value per row and cannot be used over the grouped result", path)
}

// aggregate reads the aggregate the grouped statement computed for the call.
func (e outerEnv) aggregate(t *formula.Typed) (keel.DynExpr, error) {
	l, ok := e.o.c.hoistLabels[hoistKey(t.Node, e.cell)]
	if !ok {
		return keel.DynExpr{}, fail(CodeNotAllowed, "", "the aggregate %s was not computed", formula.Format(t.Node))
	}
	return e.o.q.Path(l), nil
}

// window computes a window column over the grouped result, partitioned by level as well, so
// subtotal rows never mix with detail rows.
func (o *outerFrame) window(p *colPlan) (keel.DynExpr, error) {
	w := p.def.Window
	x, err := o.column(o.c.byID[w.Of], "")
	if err != nil {
		return keel.DynExpr{}, err
	}
	var parts []keel.Expr
	if len(o.c.sets) > 0 {
		parts = append(parts, o.q.Path(levelLabel))
	}
	for _, id := range w.Partition {
		e, err := o.column(o.c.byID[id], "")
		if err != nil {
			return keel.DynExpr{}, err
		}
		parts = append(parts, e)
	}
	base := keel.PartitionBy(parts...)
	var ordered keel.Window
	if w.Over != "" {
		over, err := o.column(o.c.byID[w.Over], "")
		if err != nil {
			return keel.DynExpr{}, err
		}
		ordered = base.OrderBy(over.Asc())
	}
	return windowExpr(w, x, base, ordered, p.out.Type), nil
}

// windowExpr is a window function over a column of the grouped result.
func windowExpr(w *ir.Window, x keel.DynExpr, base, ordered keel.Window, t formula.Type) keel.DynExpr {
	offset := max(w.Offset, 1)
	switch w.Fn {
	case ir.WindowRunningTotal:
		return castTo(x.Sum().Over(ordered.Rows(keel.UnboundedPreceding, keel.CurrentRow)), t)
	case ir.WindowPercentOfTotal:
		return x.Cast(keel.TypeNumeric).Div(x.Sum().Over(base).NullIf(0))
	case ir.WindowRank, ir.WindowDenseRank:
		order := x.Asc()
		if w.Desc {
			order = x.Desc()
		}
		fn := keel.Rank()
		if w.Fn == ir.WindowDenseRank {
			fn = keel.DenseRank()
		}
		return keel.DynOf(fn).Over(base.OrderBy(order))
	case ir.WindowMovingAvg:
		frame := ordered.Rows(keel.Preceding(int64(w.Size-1)), keel.CurrentRow)
		if t == formula.Float {
			return x.Cast(keel.TypeFloat64).Avg().Over(frame)
		}
		return x.Cast(keel.TypeNumeric).Avg().Over(frame)
	case ir.WindowChange:
		return x.Sub(keel.DynOf(keel.Lag(x, offset, nil)).Over(ordered))
	case ir.WindowPercentChange:
		prev := keel.DynOf(keel.Lag(x, offset, nil)).Over(ordered)
		return x.Sub(prev).Cast(keel.TypeNumeric).Div(prev.NullIf(0))
	case ir.WindowFirst:
		return keel.DynOf(keel.FirstValue(x)).Over(ordered.Rows(keel.UnboundedPreceding, keel.UnboundedFollowing))
	}
	return keel.DynOf(keel.LastValue(x)).Over(ordered.Rows(keel.UnboundedPreceding, keel.UnboundedFollowing))
}
