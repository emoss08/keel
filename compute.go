package keel

import (
	"github.com/emoss08/keel/schema"
)

// compute is a computed field a query fills: the column of the field and the expression selected
// into it, or the error that makes the query fail.
type compute struct {
	col int32
	e   Expr
	err error
}

// Compute selects e into the computed field col of the rows the query returns: a search rank, an
// aggregate over related rows, a value of a joined table. col must be a field tagged computed and
// e's type must fit it (an int64 field takes count(*), a float64 field ts_rank, a string field a
// string expression); expressions whose type keel cannot tell, such as Raw, are checked by the
// driver when rows decode. The query can filter and order by col too: keel writes e in its place.
//
//	s.Compute(s.Rank, s.SearchVector.Rank(q)).Where(s.SearchVector.Match(q)).Order(s.Rank.Desc())
func (q *Query[T]) Compute(col Column, e Expr) *Query[T] {
	q.q.computes = append(q.q.computes, newCompute(q.q.ref, col.column(), e, q.q.computes))
	return q
}

// Compute starts a query that fills the computed field col with e.
func (h Handle[T]) Compute(col Column, e Expr) *Query[T] {
	return h.Query().Compute(col, e)
}

// newCompute checks that c is a computed field of ref's table, not computed yet, and that e fits
// it.
func newCompute(ref *Ref, c Col, e Expr, prev []compute) compute {
	out := compute{col: c.Index, e: e}
	t := ref.Table
	switch {
	case c.Ref == nil || c.Ref.Table != t:
		out.err = errf("Compute fills a field of %s, not of the query's table %s", tableName(c.Ref), t.Name)
	case t.Columns[c.Index].Stored():
		out.err = errf("%s.%s is a column; Compute fills fields tagged computed", t.Name, t.Columns[c.Index].Name)
	case e == nil:
		out.err = errf("Compute %s.%s: nil expression", t.Name, t.Columns[c.Index].Name)
	default:
		out.err = checkFits(&t.Columns[c.Index], t.Name, e)
	}
	for _, p := range prev {
		if out.err == nil && p.col == c.Index {
			out.err = errf("%s.%s is computed twice", t.Name, t.Columns[c.Index].Name)
		}
	}
	return out
}

// tableName names the table of ref for an error message.
func tableName(ref *Ref) string {
	if ref == nil || ref.Table == nil {
		return "no table"
	}
	return ref.Table.Name
}

// checkFits refuses an expression whose value kind keel knows and that does not fit column c.
func checkFits(c *schema.Column, table string, e Expr) error {
	k, known := exprKind(e)
	if !known || kindFits(c.Kind, k) {
		return nil
	}
	return errf("Compute %s.%s: the field is %s, the expression is %s", table, c.Name, kindNames[c.Kind],
		kindNames[k])
}

// kindNames name value kinds in error messages.
var kindNames = map[schema.Kind]string{
	schema.KindOther: "numeric or a custom type", schema.KindString: "text", schema.KindInt16: "smallint",
	schema.KindInt32: "integer", schema.KindInt64: "bigint", schema.KindFloat32: "real",
	schema.KindFloat64: "double precision", schema.KindBool: "boolean", schema.KindBytes: "bytea",
	schema.KindTime: "a time", schema.KindJSON: "jsonb", schema.KindStringArray: "text[]",
	schema.KindInt64Array: "bigint[]", schema.KindTSVector: "tsvector",
}

// kindFits reports whether a value of kind k decodes into a field of kind field without loss: equal
// kinds, wider integers, floats from any number, and numeric scanners (decimals) from any number.
func kindFits(field, k schema.Kind) bool {
	if field == k {
		return true
	}
	switch field {
	case schema.KindInt64:
		return k == schema.KindInt32 || k == schema.KindInt16
	case schema.KindInt32:
		return k == schema.KindInt16
	case schema.KindFloat64:
		return numericKind(k)
	case schema.KindFloat32:
		return k == schema.KindInt16
	case schema.KindOther:
		return numericKind(k)
	case schema.KindString:
		return k == schema.KindTSVector
	}
	return false
}

// numericKind reports the kinds that hold numbers; KindOther is numeric in expression kinds, as
// keel gives it only to sum and avg over numbers.
func numericKind(k schema.Kind) bool {
	switch k {
	case schema.KindInt16, schema.KindInt32, schema.KindInt64, schema.KindFloat32, schema.KindFloat64,
		schema.KindOther:
		return true
	}
	return false
}

// exprKind returns the kind of value e yields when keel knows it: a column's kind, text for
// string expressions, and the result kind of the aggregates and window functions keel builds.
func exprKind(e Expr) (schema.Kind, bool) {
	switch x := e.(type) {
	case DynExpr:
		return dynKind(x)
	case StringExpr:
		return schema.KindString, true
	case scalarExpr:
		return x.kind()
	case Agg:
		return aggKind(x)
	case columnar:
		c := x.column()
		if c.Ref == nil || c.Ref.Table == nil || c.Ref.Table.Columns[c.Index].Kind == schema.KindOther {
			return 0, false
		}
		return c.Ref.Table.Columns[c.Index].Kind, true
	}
	return 0, false
}

// dynKind is exprKind of a dynamic expression: a column or an aggregate.
func dynKind(d DynExpr) (schema.Kind, bool) {
	switch {
	case d.n == nil:
		return 0, false
	case d.n.col != nil:
		return exprKind(*d.n.col)
	case d.n.agg != nil:
		return aggKind(*d.n.agg)
	}
	if a, ok := d.n.e.(Agg); ok {
		return aggKind(a)
	}
	return 0, false
}

// aggKinds are the result kinds of aggregates and window functions whose kind does not depend on
// their argument.
var aggKinds = map[string]schema.Kind{
	"count": schema.KindInt64, "row_number": schema.KindInt64, "rank": schema.KindInt64,
	"dense_rank": schema.KindInt64, "ntile": schema.KindInt32, "percent_rank": schema.KindFloat64,
	"cume_dist": schema.KindFloat64, "ts_rank": schema.KindFloat32, "ts_rank_cd": schema.KindFloat32,
	"cardinality": schema.KindInt32, "GROUPING": schema.KindInt32,
	"bool_and": schema.KindBool, "bool_or": schema.KindBool,
}

// aggKind returns the result kind of an aggregate: fixed for counts, ranks and the functions of
// aggKinds; the argument's kind for min, max and the value window functions; bigint for sum over
// smaller integers, numeric for sum over bigint and avg over integers, double precision for avg
// over floats.
func aggKind(a Agg) (schema.Kind, bool) {
	if k, ok := aggKinds[a.fn]; ok {
		return k, true
	}
	if a.arg == nil {
		return 0, false
	}
	arg, known := exprKind(a.arg)
	if !known {
		return 0, false
	}
	switch a.fn {
	case "min", "max", "first_value", "last_value", "nth_value", "lag", "lead":
		return arg, true
	case "sum":
		switch arg {
		case schema.KindInt16, schema.KindInt32:
			return schema.KindInt64, true
		case schema.KindInt64:
			return schema.KindOther, true
		case schema.KindFloat32, schema.KindFloat64:
			return arg, true
		}
	case "avg":
		switch arg {
		case schema.KindInt16, schema.KindInt32, schema.KindInt64:
			return schema.KindOther, true
		case schema.KindFloat32, schema.KindFloat64:
			return schema.KindFloat64, true
		}
	}
	return 0, false
}

// Scalar is the value of a query that picks one expression and returns at most one row, as an
// expression: (SELECT …). A query that references the outer statement's columns is correlated, so
// Scalar fills a computed field with an aggregate over related rows:
//
//	c.Compute(c.Loads, keel.Scalar(s.Pick(keel.Count()).Where(s.CustomerID.EqCol(c.CustomerID))))
//
// The subquery is scoped like any statement: it gets its tables' tenant predicates.
func Scalar(q subquery) Expr { return scalarExpr{q: q} }

// ArrayOf is the array of the values of a query that picks one expression: ARRAY(SELECT …), an
// empty array when the query returns no rows, where array_agg would be NULL. A correlated query
// collects a row's related values, distinct with Distinct:
//
//	keel.ArrayOf(st.Pick(st.City).Where(st.ShipmentID.EqCol(s.ID)).Distinct())
func ArrayOf(q subquery) Expr { return arrayExpr{q: q} }

type arrayExpr struct{ q subquery }

func (e arrayExpr) writeSQL(r *render) {
	r.s("ARRAY(")
	e.q.writeSubquery(r, false)
	r.b(')')
}

// scalarExpr is a scalar subquery.
type scalarExpr struct{ q subquery }

// writeSQL writes (SELECT …).
func (e scalarExpr) writeSQL(r *render) {
	r.b('(')
	e.q.writeSubquery(r, false)
	r.b(')')
}

// kind returns the kind of the one expression a typed query picks.
func (e scalarExpr) kind() (schema.Kind, bool) {
	p, ok := e.q.(interface{ picked() []Expr })
	if !ok || len(p.picked()) != 1 {
		return 0, false
	}
	return exprKind(p.picked()[0])
}

// picked returns the expressions the query picks.
func (q *Query[T]) picked() []Expr { return q.q.exprs }

// scopedCompute is a computed field of a statement: the reference whose rows it fills, the field,
// the expression written wherever the statement uses the field, and whether that expression is
// being written, which refuses an expression that reads its own field.
type scopedCompute struct {
	ref     *Ref
	col     int32
	e       Expr
	writing bool
}

// registerComputes records the computed fields of a statement's nodes on its scope, so their
// columns, wherever the statement uses them, write the expressions.
func (r *render) registerComputes(sc *scope, nodes []*lnode) {
	for _, n := range nodes {
		if n.spec == nil {
			continue
		}
		for _, c := range n.spec.computes {
			if c.err != nil {
				r.fail(c.err)
				continue
			}
			sc.computed = append(sc.computed, &scopedCompute{ref: n.ref, col: c.col, e: c.e})
		}
	}
}

// computed writes the expression a statement selects into computed field c, (e), from the
// innermost scope that computes it; a column of a table's base handle matches the computed field
// of the table's innermost reference, as alias does. It fails when no scope computes the field.
func (r *render) computed(c Col) {
	sc := r.findCompute(c)
	if sc == nil {
		r.fail(errf("%s.%s is computed; fill it with Compute before using it", c.Ref.Table.Name,
			c.Ref.Table.Columns[c.Index].Name))
		r.s("NULL")
		return
	}
	if sc.writing {
		r.fail(errf("the expression of %s.%s reads the field itself", c.Ref.Table.Name,
			c.Ref.Table.Columns[c.Index].Name))
		return
	}
	sc.writing = true
	r.b('(')
	sc.e.writeSQL(r)
	r.b(')')
	sc.writing = false
}

// findCompute returns the computed field c refers to, or nil.
func (r *render) findCompute(c Col) *scopedCompute {
	for s := r.sc; s != nil; s = s.parent {
		for _, sc := range s.computed {
			if sc.ref == c.Ref && sc.col == c.Index {
				return sc
			}
		}
	}
	if c.Ref.isPath() || c.Ref.alias != "" {
		return nil
	}
	for s := r.sc; s != nil; s = s.parent {
		for _, sc := range s.computed {
			if sc.ref.Table == c.Ref.Table && sc.col == c.Index {
				return sc
			}
		}
	}
	return nil
}
