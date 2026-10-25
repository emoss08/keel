package keel

import (
	"unsafe"
)

// DynExpr is an expression of a dynamic query, built at run time: a column or relation path
// resolved by name against the schema, a literal bound as a parameter, or an operator, function,
// CASE, CAST, aggregate or window over other expressions. It is a Pred, so Where, Having, And, Or
// and Filter take it, and an Expr, so it mixes with keel's typed expressions.
//
// A name that does not resolve, a function outside the allow-list or a misplaced Filter makes an
// expression that carries an error: Err reports it, and a statement that uses the expression
// fails with it. Identifiers in the SQL only ever come from the schema; every value is a bound
// parameter.
type DynExpr struct{ n *dynNode }

// dynNode is one DynExpr. base is the node an As copy was made from, so a relabelled expression
// still matches its original in Order and keyset pages. col is set for a bare column, agg for an
// aggregate or window function.
type dynNode struct {
	e     Expr
	base  *dynNode
	col   *Col
	agg   *Agg
	label string
	err   error
}

func newDyn(e Expr, label string) DynExpr {
	n := &dynNode{e: e, label: label}
	n.base = n
	return DynExpr{n: n}
}

func dynFail(err error) DynExpr {
	n := &dynNode{err: err}
	n.base = n
	return DynExpr{n: n}
}

func dynColumn(c Col, label string) DynExpr {
	d := newDyn(c, label)
	d.n.col = &c
	return d
}

func dynAgg(a Agg, label string, errs ...error) DynExpr {
	for _, err := range errs {
		if err != nil {
			return dynFail(err)
		}
	}
	d := newDyn(a, label)
	d.n.agg = &a
	return d
}

func (d DynExpr) writeSQL(r *render) {
	switch {
	case d.n == nil:
		r.fail(errf("empty DynExpr"))
	case d.n.err != nil:
		r.fail(d.n.err)
	default:
		d.n.e.writeSQL(r)
	}
}

func (DynExpr) pred() {}

// Err returns the error the expression carries: an unresolved name, a function outside the
// allow-list, a misplaced Filter or Over. A statement using the expression fails with it.
func (d DynExpr) Err() error {
	if d.n == nil {
		return errf("empty DynExpr")
	}
	return d.n.err
}

// Label is the name the expression's result column gets: the one given with As, else the path a
// column was resolved from ("customer.name"), else the function or aggregate name.
func (d DynExpr) Label() string {
	if d.n == nil {
		return ""
	}
	return d.n.label
}

// As returns the expression with label as its result column's name. The label is kept in Go and
// never written into the SQL, so it may be any text.
func (d DynExpr) As(label string) DynExpr {
	if d.n == nil {
		return d
	}
	c := *d.n
	c.label = label
	return DynExpr{n: &c}
}

// same reports whether d and o are the same expression, one possibly relabelled with As.
func (d DynExpr) same(o DynExpr) bool { return d.n != nil && o.n != nil && d.n.base == o.n.base }

// DynLit binds v as a parameter. A nil v is NULL. Postgres infers the parameter's type from where
// it is used; Cast fixes it where the context does not (a literal on both sides of an operator,
// a CASE result).
func DynLit(v any) DynExpr {
	if d, ok := v.(DynExpr); ok {
		return d
	}
	if e, ok := v.(Expr); ok {
		return DynOf(e)
	}
	if v == nil {
		return newDyn(rawText("NULL"), "null")
	}
	l := &litExpr{v: v}
	return newDyn(l, "value")
}

// litExpr is a bound value. Its address identifies it, so a literal written twice in a statement
// binds once.
type litExpr struct{ v any }

func (l *litExpr) writeSQL(r *render) { r.argSlot(l.v, unsafe.Pointer(l), "") }

// rawText is SQL text keel writes itself.
type rawText string

func (t rawText) writeSQL(r *render) { r.s(string(t)) }

// DynOf wraps one of keel's typed expressions (a column of a generated handle, an Agg such as
// RowNumber or Grouping, a Raw) as a DynExpr. An Agg keeps Filter and Over.
func DynOf(e Expr) DynExpr {
	switch x := e.(type) {
	case nil:
		return dynFail(errf("DynOf of a nil expression"))
	case DynExpr:
		return x
	case Agg:
		return dynAgg(x, x.fn)
	case Col:
		return dynColumn(x, x.Ref.Table.Columns[x.Index].Name)
	case columnar:
		c := x.column()
		return dynColumn(c, c.Ref.Table.Columns[c.Index].Name)
	}
	return newDyn(e, "expr")
}

// dynOperand turns v into an expression: a DynExpr or Expr as it is, nil as NULL, any other value
// a literal.
func dynOperand(v any) (Expr, error) {
	switch x := v.(type) {
	case DynExpr:
		return x, x.Err()
	case Expr:
		return x, nil
	case nil:
		return rawText("NULL"), nil
	}
	return &litExpr{v: v}, nil
}

// rhs is the right operand of d op v. A value compared with a bare column shares its placeholder
// with an equal value compared with the same column through the same operator.
func (d DynExpr) rhs(op string, v any) (Expr, error) {
	if d.n != nil && d.n.col != nil {
		switch v.(type) {
		case DynExpr, Expr, nil:
		default:
			return colValue{c: *d.n.col, op: op, v: v}, nil
		}
	}
	return dynOperand(v)
}

// colValue is a value compared with a column.
type colValue struct {
	c  Col
	op string
	v  any
}

func (c colValue) writeSQL(r *render) { r.argCol(c.v, c.c, c.op) }

// binOp is (a op b).
type binOp struct {
	a, b Expr
	op   string
}

func (e binOp) writeSQL(r *render) {
	r.b('(')
	e.a.writeSQL(r)
	r.s(e.op)
	e.b.writeSQL(r)
	r.b(')')
}

func (d DynExpr) bin(op, label string, v any) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	if c := d.nullZeroCol(v); c != nil {
		if p := nullZeroCmp(*c, op, v); p != nil {
			return newDyn(p, label)
		}
	}
	b, err := d.rhs(op, v)
	if err != nil {
		return dynFail(err)
	}
	return newDyn(binOp{a: d, b: b, op: op}, label)
}

// Eq is d = v. v is a DynExpr, an Expr or a value bound as a parameter, as for every operator.
func (d DynExpr) Eq(v any) DynExpr { return d.bin(" = ", d.Label(), v) }

// Ne is d <> v.
func (d DynExpr) Ne(v any) DynExpr { return d.bin(" <> ", d.Label(), v) }

// Gt is d > v.
func (d DynExpr) Gt(v any) DynExpr { return d.bin(" > ", d.Label(), v) }

// Gte is d >= v.
func (d DynExpr) Gte(v any) DynExpr { return d.bin(" >= ", d.Label(), v) }

// Lt is d < v.
func (d DynExpr) Lt(v any) DynExpr { return d.bin(" < ", d.Label(), v) }

// Lte is d <= v.
func (d DynExpr) Lte(v any) DynExpr { return d.bin(" <= ", d.Label(), v) }

// IsDistinctFrom is d IS DISTINCT FROM v: inequality that treats NULL as a value.
func (d DynExpr) IsDistinctFrom(v any) DynExpr {
	return d.bin(" IS DISTINCT FROM ", d.Label(), v)
}

// Like is d LIKE pattern, with % and _ as given.
func (d DynExpr) Like(pattern any) DynExpr { return d.bin(" LIKE ", d.Label(), pattern) }

// ILike is d ILIKE pattern.
func (d DynExpr) ILike(pattern any) DynExpr { return d.bin(" ILIKE ", d.Label(), pattern) }

// Contains is d LIKE '%s%' with % and _ in s matching themselves.
func (d DynExpr) Contains(s string) DynExpr {
	return d.bin(" LIKE ", d.Label(), "%"+escapeLike(s)+"%")
}

// ContainsFold is Contains ignoring case.
func (d DynExpr) ContainsFold(s string) DynExpr {
	return d.bin(" ILIKE ", d.Label(), "%"+escapeLike(s)+"%")
}

// HasPrefix is d LIKE 's%' with s escaped.
func (d DynExpr) HasPrefix(s string) DynExpr {
	return d.bin(" LIKE ", d.Label(), escapeLike(s)+"%")
}

// HasSuffix is d LIKE '%s' with s escaped.
func (d DynExpr) HasSuffix(s string) DynExpr {
	return d.bin(" LIKE ", d.Label(), "%"+escapeLike(s))
}

// In is d = ANY(list): list is a slice ([]string, []int64, []any of one type) bound as one array
// parameter, so the statement text does not depend on its length.
func (d DynExpr) In(list any) DynExpr { return d.anyOf(list, " = ANY(") }

// NotIn is d <> ALL(list).
func (d DynExpr) NotIn(list any) DynExpr { return d.anyOf(list, " <> ALL(") }

// nullZeroCol returns the column of d when d is a bare nullzero column compared with the value v,
// so the comparison follows nullzero's meaning of the zero value.
func (d DynExpr) nullZeroCol(v any) *Col {
	if d.n == nil || d.n.col == nil || !d.n.col.nullZero() {
		return nil
	}
	switch v.(type) {
	case DynExpr, Expr, nil:
		return nil
	}
	return d.n.col
}

func (d DynExpr) anyOf(list any, op string) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	if c := d.nullZeroCol(list); c != nil {
		if p := nullZeroAny(*c, list, op == " <> ALL("); p != nil {
			return newDyn(p, d.Label())
		}
	}
	b, err := dynOperand(list)
	if err != nil {
		return dynFail(err)
	}
	return newDyn(exprFunc(func(r *render) {
		r.b('(')
		d.writeSQL(r)
		r.s(op)
		b.writeSQL(r)
		r.s("))")
	}), d.Label())
}

// Between is d BETWEEN lo AND hi.
func (d DynExpr) Between(lo, hi any) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	a, err := d.rhs(" BETWEEN ", lo)
	if err != nil {
		return dynFail(err)
	}
	b, err := d.rhs(" BETWEEN ", hi)
	if err != nil {
		return dynFail(err)
	}
	return newDyn(exprFunc(func(r *render) {
		r.b('(')
		d.writeSQL(r)
		r.s(" BETWEEN ")
		a.writeSQL(r)
		r.s(" AND ")
		b.writeSQL(r)
		r.b(')')
	}), d.Label())
}

func (d DynExpr) suffix(s string) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	return newDyn(exprFunc(func(r *render) {
		r.b('(')
		d.writeSQL(r)
		r.s(s)
		r.b(')')
	}), d.Label())
}

// IsNull is d IS NULL.
func (d DynExpr) IsNull() DynExpr { return d.suffix(" IS NULL") }

// IsNotNull is d IS NOT NULL.
func (d DynExpr) IsNotNull() DynExpr { return d.suffix(" IS NOT NULL") }

// IsTrue is d IS TRUE.
func (d DynExpr) IsTrue() DynExpr { return d.suffix(" IS TRUE") }

// IsFalse is d IS FALSE.
func (d DynExpr) IsFalse() DynExpr { return d.suffix(" IS FALSE") }

// And is d AND ps…; nil predicates are skipped.
func (d DynExpr) And(ps ...Pred) DynExpr { return d.logic(" AND ", ps) }

// Or is d OR ps…; nil predicates are skipped.
func (d DynExpr) Or(ps ...Pred) DynExpr { return d.logic(" OR ", ps) }

func (d DynExpr) logic(op string, ps []Pred) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	for _, p := range ps {
		if x, ok := p.(DynExpr); ok && x.Err() != nil {
			return dynFail(x.Err())
		}
	}
	all := append([]Pred{d}, ps...)
	return newDyn(boolList{ps: all, op: op, empty: "TRUE"}, d.Label())
}

// Not is NOT d.
func (d DynExpr) Not() DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	return newDyn(exprFunc(func(r *render) {
		r.s("(NOT ")
		d.writeSQL(r)
		r.b(')')
	}), d.Label())
}

// Add is d + v.
func (d DynExpr) Add(v any) DynExpr { return d.bin(" + ", d.Label(), v) }

// Sub is d - v.
func (d DynExpr) Sub(v any) DynExpr { return d.bin(" - ", d.Label(), v) }

// Mul is d * v.
func (d DynExpr) Mul(v any) DynExpr { return d.bin(" * ", d.Label(), v) }

// Div is d / v. Integer division truncates; Cast one side to TypeNumeric or TypeFloat64 for a
// fraction, and wrap the divisor with NullIf(0) where it can be zero.
func (d DynExpr) Div(v any) DynExpr { return d.bin(" / ", d.Label(), v) }

// Mod is d % v.
func (d DynExpr) Mod(v any) DynExpr { return d.bin(" % ", d.Label(), v) }

// Concat is d || v.
func (d DynExpr) Concat(v any) DynExpr { return d.bin(" || ", d.Label(), v) }

// Neg is -d.
func (d DynExpr) Neg() DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	return newDyn(exprFunc(func(r *render) {
		r.s("(-")
		d.writeSQL(r)
		r.b(')')
	}), d.Label())
}

// Coalesce is coalesce(d, vs…).
func (d DynExpr) Coalesce(vs ...any) DynExpr {
	return DynFunc("coalesce", append([]any{d}, vs...)...).As(d.Label())
}

// NullIf is nullif(d, v): NULL when d equals v, for safe division.
func (d DynExpr) NullIf(v any) DynExpr { return DynFunc("nullif", d, v).As(d.Label()) }

// Cast is CAST(d AS t). A literal is cast as $n::t, and equal literals cast to the same type share
// one parameter.
func (d DynExpr) Cast(t ValueType) DynExpr {
	if err := d.Err(); err != nil {
		return dynFail(err)
	}
	typ := t.sqlType()
	if typ == "" {
		return dynFail(errf("cannot cast to value type %d", t))
	}
	if l, ok := d.n.e.(*litExpr); ok {
		return newDyn(exprFunc(func(r *render) {
			r.argTyped(l.v, typ)
			r.s("::")
			r.s(typ)
		}), d.Label())
	}
	return newDyn(exprFunc(func(r *render) {
		r.s("CAST(")
		d.writeSQL(r)
		r.s(" AS ")
		r.s(typ)
		r.b(')')
	}), d.Label())
}

// Asc orders by d ascending.
func (d DynExpr) Asc() Order { return Order{e: d, col: d.column()} }

// Desc orders by d descending.
func (d DynExpr) Desc() Order { return Order{e: d, col: d.column(), desc: true} }

// column returns the column a bare column expression reads, or nil.
func (d DynExpr) column() *Col {
	if d.n == nil || d.n.err != nil {
		return nil
	}
	return d.n.col
}

// Sum is sum(d).
func (d DynExpr) Sum() DynExpr { return dynAgg(Agg{fn: "sum", arg: d}, d.Label(), d.Err()) }

// Avg is avg(d).
func (d DynExpr) Avg() DynExpr { return dynAgg(Agg{fn: "avg", arg: d}, d.Label(), d.Err()) }

// Min is min(d).
func (d DynExpr) Min() DynExpr { return dynAgg(Agg{fn: "min", arg: d}, d.Label(), d.Err()) }

// Max is max(d).
func (d DynExpr) Max() DynExpr { return dynAgg(Agg{fn: "max", arg: d}, d.Label(), d.Err()) }

// Count is count(d): the rows where d is not NULL.
func (d DynExpr) Count() DynExpr { return dynAgg(Agg{fn: "count", arg: d}, d.Label(), d.Err()) }

// CountDistinct is count(DISTINCT d).
func (d DynExpr) CountDistinct() DynExpr {
	return dynAgg(Agg{fn: "count", arg: d, distinct: true}, d.Label(), d.Err())
}

// DynCount is count(*).
func DynCount() DynExpr { return dynAgg(Count(), "count") }

// Filter restricts the rows an aggregate sees: agg FILTER (WHERE p). d must be an aggregate.
func (d DynExpr) Filter(p Pred) DynExpr {
	if err := d.Err(); err != nil {
		return d
	}
	if d.n.agg == nil {
		return dynFail(errf("Filter needs an aggregate, %s is not one", d.Label()))
	}
	if x, ok := p.(DynExpr); ok && x.Err() != nil {
		return dynFail(x.Err())
	}
	return dynAgg(d.n.agg.Filter(p), d.Label())
}

// Over turns an aggregate or window function into a window function over w: a running or moving
// aggregate, a rank, an offset or a frame value.
func (d DynExpr) Over(w Window) DynExpr {
	if err := d.Err(); err != nil {
		return d
	}
	if d.n.agg == nil {
		return dynFail(errf("Over needs an aggregate or window function, %s is not one", d.Label()))
	}
	return dynAgg(d.n.agg.OverWindow(w), d.Label())
}

// dynFunc describes a function of the allow-list: its argument count bounds (max -1 for any) and
// whether it aggregates.
type dynFunc struct {
	min, max int
	agg      bool
}

// dynFuncs is the allow-list of functions DynFunc calls.
var dynFuncs = map[string]dynFunc{
	"abs": {1, 1, false}, "ceil": {1, 1, false}, "floor": {1, 1, false}, "round": {1, 2, false},
	"trunc": {1, 2, false}, "sign": {1, 1, false}, "sqrt": {1, 1, false}, "power": {2, 2, false},
	"mod": {2, 2, false}, "greatest": {1, -1, false}, "least": {1, -1, false},
	"coalesce": {1, -1, false}, "nullif": {2, 2, false},
	"lower": {1, 1, false}, "upper": {1, 1, false}, "initcap": {1, 1, false}, "trim": {1, 1, false},
	"ltrim": {1, 2, false}, "rtrim": {1, 2, false}, "btrim": {1, 2, false}, "length": {1, 1, false},
	"substr": {2, 3, false}, "left": {2, 2, false}, "right": {2, 2, false},
	"replace": {3, 3, false}, "concat": {1, -1, false}, "concat_ws": {2, -1, false},
	"strpos": {2, 2, false}, "split_part": {3, 3, false}, "lpad": {2, 3, false},
	"rpad": {2, 3, false}, "md5": {1, 1, false}, "to_char": {2, 2, false},
	"date_trunc": {2, 3, false}, "date_part": {2, 2, false}, "date_bin": {2, 3, false},
	"age": {1, 2, false}, "to_timestamp": {1, 1, false}, "timezone": {2, 2, false},
	"make_date": {3, 3, false}, "now": {0, 0, false}, "jsonb_extract_path_text": {2, -1, false},
	"jsonb_typeof": {1, 1, false}, "jsonb_array_length": {1, 1, false},
	"cardinality": {1, 1, false}, "array_to_string": {2, 3, false},
	"sum": {1, 1, true}, "avg": {1, 1, true}, "min": {1, 1, true}, "max": {1, 1, true},
	"count": {0, 1, true}, "bool_and": {1, 1, true}, "bool_or": {1, 1, true},
	"string_agg": {2, 2, true}, "stddev": {1, 1, true}, "stddev_samp": {1, 1, true},
	"stddev_pop": {1, 1, true}, "variance": {1, 1, true}, "var_samp": {1, 1, true},
	"var_pop": {1, 1, true},
}

// DynFunc calls the function name of keel's allow-list with args: DynExprs, Exprs or values bound
// as parameters. Scalar functions: abs, ceil, floor, round, trunc, sign, sqrt, power, mod,
// greatest, least, coalesce, nullif, lower, upper, initcap, trim, ltrim, rtrim, btrim, length,
// substr, left, right, replace, concat, concat_ws, strpos, split_part, lpad, rpad, md5, to_char,
// date_trunc, date_part, date_bin, age, to_timestamp, timezone, make_date, now,
// jsonb_extract_path_text, jsonb_typeof, jsonb_array_length, cardinality, array_to_string.
// Aggregates, which take Filter and Over: sum, avg, min, max, count (count() is count(*)),
// bool_and, bool_or, string_agg, stddev, stddev_samp, stddev_pop, variance, var_samp, var_pop.
// Any other name makes an expression that fails.
func DynFunc(name string, args ...any) DynExpr {
	f, ok := dynFuncs[name]
	if !ok {
		return dynFail(errf("function %q is not in the dynamic query allow-list", name))
	}
	if len(args) < f.min || (f.max >= 0 && len(args) > f.max) {
		return dynFail(errf("%s takes %d to %d arguments, got %d", name, f.min, f.max, len(args)))
	}
	ops := make([]Expr, len(args))
	for i, a := range args {
		e, err := dynOperand(a)
		if err != nil {
			return dynFail(err)
		}
		ops[i] = e
	}
	if f.agg {
		if len(ops) == 0 {
			return dynAgg(Agg{fn: name, star: true}, name)
		}
		a := Agg{fn: name, arg: ops[0]}
		for _, o := range ops[1:] {
			a.ops = append(a.ops, o)
		}
		return dynAgg(a, name)
	}
	return newDyn(exprFunc(func(r *render) {
		r.s(name)
		r.b('(')
		writeExprs(r, ops)
		r.b(')')
	}), name)
}

// DynRow is the row value ROW(vs…): DynExprs, Exprs or values bound as parameters. Rows compare
// element by element in order, so DynRow(a, b).Gt(DynRow(x, y)) is the keyset condition of a page
// ordered by a, b ascending, which an index on (a, b) serves.
func DynRow(vs ...any) DynExpr {
	if len(vs) < 2 {
		return dynFail(errf("DynRow needs at least two values, got %d", len(vs)))
	}
	ops := make([]Expr, len(vs))
	for i, v := range vs {
		e, err := dynOperand(v)
		if err != nil {
			return dynFail(err)
		}
		ops[i] = e
	}
	return newDyn(exprFunc(func(r *render) {
		r.s("ROW(")
		writeExprs(r, ops)
		r.b(')')
	}), "row")
}

// DynCaseBuilder builds CASE WHEN … THEN … [ELSE …] END. Start it with DynCase.
type DynCaseBuilder struct {
	whens []dynWhen
	err   error
}

type dynWhen struct {
	cond Pred
	then Expr
}

// DynCase starts a CASE expression.
func DynCase() DynCaseBuilder { return DynCaseBuilder{} }

// When adds WHEN cond THEN then; then is a DynExpr, an Expr or a value bound as a parameter.
func (b DynCaseBuilder) When(cond Pred, then any) DynCaseBuilder {
	e, err := dynOperand(then)
	if err == nil {
		if x, ok := cond.(DynExpr); ok {
			err = x.Err()
		}
	}
	if err != nil && b.err == nil {
		b.err = err
	}
	b.whens = append(append([]dynWhen(nil), b.whens...), dynWhen{cond: cond, then: e})
	return b
}

// Else ends the CASE with ELSE v.
func (b DynCaseBuilder) Else(v any) DynExpr {
	e, err := dynOperand(v)
	if err != nil {
		return dynFail(err)
	}
	return b.end(e)
}

// End ends the CASE without ELSE: rows no WHEN matches are NULL.
func (b DynCaseBuilder) End() DynExpr { return b.end(nil) }

func (b DynCaseBuilder) end(els Expr) DynExpr {
	if b.err != nil {
		return dynFail(b.err)
	}
	if len(b.whens) == 0 {
		return dynFail(errf("CASE needs at least one When"))
	}
	whens := b.whens
	return newDyn(exprFunc(func(r *render) {
		r.s("CASE")
		for _, w := range whens {
			r.s(" WHEN ")
			w.cond.writeSQL(r)
			r.s(" THEN ")
			w.then.writeSQL(r)
		}
		if els != nil {
			r.s(" ELSE ")
			els.writeSQL(r)
		}
		r.s(" END")
	}), "case")
}
