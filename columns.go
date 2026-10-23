package keel

import (
	"time"
)

// Col references one column of a table. Generated code builds every column statically; the typed
// column kinds embed it.
type Col struct {
	Ref   *Ref
	Index int32
}

func (c Col) writeSQL(r *render) { r.col(c) }

func (c Col) column() Col { return c }

// columnar is implemented by every column kind, so Pick and Save accept any of them.
type columnar interface {
	Expr
	column() Col
}

// IsNull is col IS NULL.
func (c Col) IsNull() Pred { return suffix(c, " IS NULL") }

// IsNotNull is col IS NOT NULL.
func (c Col) IsNotNull() Pred { return suffix(c, " IS NOT NULL") }

// Asc orders by the column ascending.
func (c Col) Asc() Order { cc := c; return Order{e: c, col: &cc} }

// Desc orders by the column descending.
func (c Col) Desc() Order { cc := c; return Order{e: c, col: &cc, desc: true} }

// ToNull sets the column to NULL.
func (c Col) ToNull() Assignment { return Assignment{col: c, e: func(r *render) { r.s("NULL") }} }

// ToDefault sets the column to its database default.
func (c Col) ToDefault() Assignment {
	return Assignment{col: c, e: func(r *render) { r.s("DEFAULT") }}
}

// ToExpr sets the column to an expression.
func (c Col) ToExpr(e Expr) Assignment { return Assignment{col: c, e: e.writeSQL} }

// Excluded is the value proposed for the column by the failed insert of an upsert.
func (c Col) Excluded() Expr {
	return exprFunc(func(r *render) {
		r.s("EXCLUDED.")
		r.s(c.Ref.Table.Columns[c.Index].Quoted)
	})
}

// EqCol is col = other, for joins and correlated subqueries.
func (c Col) EqCol(other Expr) Pred { return binExpr(c, " = ", other) }

// NeCol is col <> other.
func (c Col) NeCol(other Expr) Pred { return binExpr(c, " <> ", other) }

// InQuery is col IN (q). q must pick exactly one column.
func (c Col) InQuery(q subquery) Pred {
	return predExpr{e: func(r *render) {
		r.col(c)
		r.s(" IN (")
		q.writeSubquery(r, false)
		r.b(')')
	}}
}

// NotInQuery is col NOT IN (q).
func (c Col) NotInQuery(q subquery) Pred {
	return predExpr{e: func(r *render) {
		r.col(c)
		r.s(" NOT IN (")
		q.writeSubquery(r, false)
		r.b(')')
	}}
}

// Count is count(col).
func (c Col) Count() Agg { return Agg{fn: "count", arg: c} }

// CountDistinct is count(DISTINCT col).
func (c Col) CountDistinct() Agg { return Agg{fn: "count", arg: c, distinct: true} }

// Min is min(col).
func (c Col) Min() Agg { return Agg{fn: "min", arg: c} }

// Max is max(col).
func (c Col) Max() Agg { return Agg{fn: "max", arg: c} }

// Coalesce is coalesce(col, v).
func (c Col) Coalesce(v any) Expr {
	return exprFunc(func(r *render) {
		r.s("coalesce(")
		r.col(c)
		r.s(", ")
		writeOperand(r, v)
		r.b(')')
	})
}

type exprFunc func(r *render)

func (f exprFunc) writeSQL(r *render) { f(r) }

func suffix(e Expr, s string) Pred {
	return predExpr{e: func(r *render) {
		e.writeSQL(r)
		r.s(s)
	}}
}

func cmp(e Expr, op string, v any) Pred {
	return predExpr{e: func(r *render) {
		e.writeSQL(r)
		r.s(op)
		r.arg(v)
	}}
}

func binExpr(a Expr, op string, b Expr) Pred {
	return predExpr{e: func(r *render) {
		a.writeSQL(r)
		r.s(op)
		b.writeSQL(r)
	}}
}

func between(e Expr, lo, hi any) Pred {
	return predExpr{e: func(r *render) {
		e.writeSQL(r)
		r.s(" BETWEEN ")
		r.arg(lo)
		r.s(" AND ")
		r.arg(hi)
	}}
}

func anyOf(e Expr, list any, not bool) Pred {
	return predExpr{e: func(r *render) {
		e.writeSQL(r)
		if not {
			r.s(" <> ALL(")
		} else {
			r.s(" = ANY(")
		}
		r.arg(list)
		r.b(')')
	}}
}

func set(c Col, v any) Assignment {
	return Assignment{col: c, e: func(r *render) { r.arg(v) }}
}

func add(c Col, v any) Assignment {
	return Assignment{col: c, e: func(r *render) {
		r.s(c.Ref.Table.Columns[c.Index].Quoted)
		r.s(" + ")
		r.arg(v)
	}}
}

// StringCol is a text, varchar, enum or other string-valued column.
type StringCol struct{ Col }

func (c StringCol) Eq(v string) Pred             { return cmp(c.Col, " = ", v) }
func (c StringCol) Ne(v string) Pred             { return cmp(c.Col, " <> ", v) }
func (c StringCol) Gt(v string) Pred             { return cmp(c.Col, " > ", v) }
func (c StringCol) Gte(v string) Pred            { return cmp(c.Col, " >= ", v) }
func (c StringCol) Lt(v string) Pred             { return cmp(c.Col, " < ", v) }
func (c StringCol) Lte(v string) Pred            { return cmp(c.Col, " <= ", v) }
func (c StringCol) Between(lo, hi string) Pred   { return between(c.Col, lo, hi) }
func (c StringCol) IsDistinctFrom(v string) Pred { return cmp(c.Col, " IS DISTINCT FROM ", v) }
func (c StringCol) In(vs ...string) Pred         { return anyOf(c.Col, vs, false) }
func (c StringCol) NotIn(vs ...string) Pred      { return anyOf(c.Col, vs, true) }
func (c StringCol) To(v string) Assignment       { return set(c.Col, v) }

// Like matches a LIKE pattern as given; % and _ keep their meaning.
func (c StringCol) Like(pattern string) Pred { return cmp(c.Col, " LIKE ", pattern) }

// ILike matches an ILIKE pattern as given.
func (c StringCol) ILike(pattern string) Pred { return cmp(c.Col, " ILIKE ", pattern) }

// Contains reports whether the column contains s. % and _ in s match themselves.
func (c StringCol) Contains(s string) Pred { return cmp(c.Col, " LIKE ", "%"+escapeLike(s)+"%") }

// ContainsFold is Contains ignoring case.
func (c StringCol) ContainsFold(s string) Pred {
	return cmp(c.Col, " ILIKE ", "%"+escapeLike(s)+"%")
}

// HasPrefix reports whether the column starts with s.
func (c StringCol) HasPrefix(s string) Pred { return cmp(c.Col, " LIKE ", escapeLike(s)+"%") }

// HasPrefixFold is HasPrefix ignoring case.
func (c StringCol) HasPrefixFold(s string) Pred {
	return cmp(c.Col, " ILIKE ", escapeLike(s)+"%")
}

// HasSuffix reports whether the column ends with s.
func (c StringCol) HasSuffix(s string) Pred { return cmp(c.Col, " LIKE ", "%"+escapeLike(s)) }

// Match matches a POSIX regular expression.
func (c StringCol) Match(re string) Pred { return cmp(c.Col, " ~ ", re) }

// MatchFold matches a POSIX regular expression ignoring case.
func (c StringCol) MatchFold(re string) Pred { return cmp(c.Col, " ~* ", re) }

// Lower is lower(col).
func (c StringCol) Lower() StringExpr {
	return StringExpr{e: exprFunc(func(r *render) {
		r.s("lower(")
		r.col(c.Col)
		r.b(')')
	})}
}

// StringExpr is a computed string value.
type StringExpr struct{ e Expr }

func (e StringExpr) writeSQL(r *render)     { e.e.writeSQL(r) }
func (e StringExpr) Eq(v string) Pred       { return cmp(e.e, " = ", v) }
func (e StringExpr) Ne(v string) Pred       { return cmp(e.e, " <> ", v) }
func (e StringExpr) In(vs ...string) Pred   { return anyOf(e.e, vs, false) }
func (e StringExpr) Like(p string) Pred     { return cmp(e.e, " LIKE ", p) }
func (e StringExpr) Contains(s string) Pred { return cmp(e.e, " LIKE ", "%"+escapeLike(s)+"%") }
func (e StringExpr) IsNull() Pred           { return suffix(e.e, " IS NULL") }
func (e StringExpr) Asc() Order             { return Order{e: e.e} }
func (e StringExpr) Desc() Order            { return Order{e: e.e, desc: true} }

// Int64Col is a bigint column.
type Int64Col struct{ Col }

func (c Int64Col) Eq(v int64) Pred             { return cmp(c.Col, " = ", v) }
func (c Int64Col) Ne(v int64) Pred             { return cmp(c.Col, " <> ", v) }
func (c Int64Col) Gt(v int64) Pred             { return cmp(c.Col, " > ", v) }
func (c Int64Col) Gte(v int64) Pred            { return cmp(c.Col, " >= ", v) }
func (c Int64Col) Lt(v int64) Pred             { return cmp(c.Col, " < ", v) }
func (c Int64Col) Lte(v int64) Pred            { return cmp(c.Col, " <= ", v) }
func (c Int64Col) Between(lo, hi int64) Pred   { return between(c.Col, lo, hi) }
func (c Int64Col) IsDistinctFrom(v int64) Pred { return cmp(c.Col, " IS DISTINCT FROM ", v) }
func (c Int64Col) In(vs ...int64) Pred         { return anyOf(c.Col, vs, false) }
func (c Int64Col) NotIn(vs ...int64) Pred      { return anyOf(c.Col, vs, true) }
func (c Int64Col) To(v int64) Assignment       { return set(c.Col, v) }
func (c Int64Col) Add(v int64) Assignment      { return add(c.Col, v) }
func (c Int64Col) Sum() Agg                    { return Agg{fn: "sum", arg: c.Col} }
func (c Int64Col) Avg() Agg                    { return Agg{fn: "avg", arg: c.Col} }

// Int32Col is an integer column.
type Int32Col struct{ Col }

func (c Int32Col) Eq(v int32) Pred           { return cmp(c.Col, " = ", v) }
func (c Int32Col) Ne(v int32) Pred           { return cmp(c.Col, " <> ", v) }
func (c Int32Col) Gt(v int32) Pred           { return cmp(c.Col, " > ", v) }
func (c Int32Col) Gte(v int32) Pred          { return cmp(c.Col, " >= ", v) }
func (c Int32Col) Lt(v int32) Pred           { return cmp(c.Col, " < ", v) }
func (c Int32Col) Lte(v int32) Pred          { return cmp(c.Col, " <= ", v) }
func (c Int32Col) Between(lo, hi int32) Pred { return between(c.Col, lo, hi) }
func (c Int32Col) In(vs ...int32) Pred       { return anyOf(c.Col, vs, false) }
func (c Int32Col) IsDistinctFrom(v int32) Pred {
	return cmp(c.Col, " IS DISTINCT FROM ", v)
}
func (c Int32Col) NotIn(vs ...int32) Pred { return anyOf(c.Col, vs, true) }
func (c Int32Col) To(v int32) Assignment  { return set(c.Col, v) }
func (c Int32Col) Add(v int32) Assignment { return add(c.Col, v) }
func (c Int32Col) Sum() Agg               { return Agg{fn: "sum", arg: c.Col} }
func (c Int32Col) Avg() Agg               { return Agg{fn: "avg", arg: c.Col} }

// Int16Col is a smallint column.
type Int16Col struct{ Col }

func (c Int16Col) Eq(v int16) Pred           { return cmp(c.Col, " = ", v) }
func (c Int16Col) Ne(v int16) Pred           { return cmp(c.Col, " <> ", v) }
func (c Int16Col) Gt(v int16) Pred           { return cmp(c.Col, " > ", v) }
func (c Int16Col) Gte(v int16) Pred          { return cmp(c.Col, " >= ", v) }
func (c Int16Col) Lt(v int16) Pred           { return cmp(c.Col, " < ", v) }
func (c Int16Col) Lte(v int16) Pred          { return cmp(c.Col, " <= ", v) }
func (c Int16Col) Between(lo, hi int16) Pred { return between(c.Col, lo, hi) }
func (c Int16Col) In(vs ...int16) Pred       { return anyOf(c.Col, vs, false) }
func (c Int16Col) IsDistinctFrom(v int16) Pred {
	return cmp(c.Col, " IS DISTINCT FROM ", v)
}
func (c Int16Col) NotIn(vs ...int16) Pred { return anyOf(c.Col, vs, true) }
func (c Int16Col) To(v int16) Assignment  { return set(c.Col, v) }
func (c Int16Col) Add(v int16) Assignment { return add(c.Col, v) }
func (c Int16Col) Sum() Agg               { return Agg{fn: "sum", arg: c.Col} }

// Float64Col is a double precision or real column.
type Float64Col struct{ Col }

func (c Float64Col) Eq(v float64) Pred           { return cmp(c.Col, " = ", v) }
func (c Float64Col) Ne(v float64) Pred           { return cmp(c.Col, " <> ", v) }
func (c Float64Col) Gt(v float64) Pred           { return cmp(c.Col, " > ", v) }
func (c Float64Col) Gte(v float64) Pred          { return cmp(c.Col, " >= ", v) }
func (c Float64Col) Lt(v float64) Pred           { return cmp(c.Col, " < ", v) }
func (c Float64Col) Lte(v float64) Pred          { return cmp(c.Col, " <= ", v) }
func (c Float64Col) Between(lo, hi float64) Pred { return between(c.Col, lo, hi) }
func (c Float64Col) IsDistinctFrom(v float64) Pred {
	return cmp(c.Col, " IS DISTINCT FROM ", v)
}
func (c Float64Col) To(v float64) Assignment  { return set(c.Col, v) }
func (c Float64Col) Add(v float64) Assignment { return add(c.Col, v) }
func (c Float64Col) Sum() Agg                 { return Agg{fn: "sum", arg: c.Col} }
func (c Float64Col) Avg() Agg                 { return Agg{fn: "avg", arg: c.Col} }

// BoolCol is a boolean column.
type BoolCol struct{ Col }

func (c BoolCol) Eq(v bool) Pred       { return cmp(c.Col, " = ", v) }
func (c BoolCol) IsTrue() Pred         { return suffix(c.Col, " IS TRUE") }
func (c BoolCol) IsFalse() Pred        { return suffix(c.Col, " IS FALSE") }
func (c BoolCol) To(v bool) Assignment { return set(c.Col, v) }

// BytesCol is a bytea column.
type BytesCol struct{ Col }

func (c BytesCol) Eq(v []byte) Pred       { return cmp(c.Col, " = ", v) }
func (c BytesCol) To(v []byte) Assignment { return set(c.Col, v) }

// TimeCol is a timestamp, timestamptz or date column.
type TimeCol struct{ Col }

func (c TimeCol) Eq(v time.Time) Pred           { return cmp(c.Col, " = ", v) }
func (c TimeCol) Ne(v time.Time) Pred           { return cmp(c.Col, " <> ", v) }
func (c TimeCol) Gt(v time.Time) Pred           { return cmp(c.Col, " > ", v) }
func (c TimeCol) Gte(v time.Time) Pred          { return cmp(c.Col, " >= ", v) }
func (c TimeCol) Lt(v time.Time) Pred           { return cmp(c.Col, " < ", v) }
func (c TimeCol) Lte(v time.Time) Pred          { return cmp(c.Col, " <= ", v) }
func (c TimeCol) Between(lo, hi time.Time) Pred { return between(c.Col, lo, hi) }
func (c TimeCol) To(v time.Time) Assignment     { return set(c.Col, v) }

// AnyCol is a column of a type keel has no dedicated kind for (numeric through a decimal type, a
// custom sql.Scanner). Generated wrappers give it typed methods.
type AnyCol struct{ Col }

func (c AnyCol) Eq(v any) Pred           { return cmp(c.Col, " = ", v) }
func (c AnyCol) Ne(v any) Pred           { return cmp(c.Col, " <> ", v) }
func (c AnyCol) Gt(v any) Pred           { return cmp(c.Col, " > ", v) }
func (c AnyCol) Gte(v any) Pred          { return cmp(c.Col, " >= ", v) }
func (c AnyCol) Lt(v any) Pred           { return cmp(c.Col, " < ", v) }
func (c AnyCol) Lte(v any) Pred          { return cmp(c.Col, " <= ", v) }
func (c AnyCol) Between(lo, hi any) Pred { return between(c.Col, lo, hi) }
func (c AnyCol) IsDistinctFrom(v any) Pred {
	return cmp(c.Col, " IS DISTINCT FROM ", v)
}

// In takes a slice of values (for example []decimal.Decimal) bound as one array parameter.
func (c AnyCol) In(slice any) Pred    { return anyOf(c.Col, slice, false) }
func (c AnyCol) NotIn(slice any) Pred { return anyOf(c.Col, slice, true) }
func (c AnyCol) To(v any) Assignment  { return set(c.Col, v) }
func (c AnyCol) Add(v any) Assignment { return add(c.Col, v) }
func (c AnyCol) Sum() Agg             { return Agg{fn: "sum", arg: c.Col} }
func (c AnyCol) Avg() Agg             { return Agg{fn: "avg", arg: c.Col} }

// JSONBCol is a jsonb column.
type JSONBCol struct{ Col }

// Contains is col @> v, with v encoded as JSON.
func (c JSONBCol) Contains(v any) Pred { return cmp(c.Col, " @> ", v) }

// HasKey is col ? key.
func (c JSONBCol) HasKey(key string) Pred { return cmp(c.Col, " ? ", key) }

// HasAnyKey is col ?| keys.
func (c JSONBCol) HasAnyKey(keys ...string) Pred { return cmp(c.Col, " ?| ", keys) }

// HasAllKeys is col ?& keys.
func (c JSONBCol) HasAllKeys(keys ...string) Pred { return cmp(c.Col, " ?& ", keys) }

// Text is col #>> path: the value at path as text.
func (c JSONBCol) Text(path ...string) StringExpr {
	return StringExpr{e: exprFunc(func(r *render) {
		r.b('(')
		r.col(c.Col)
		r.s(" #>> ")
		r.arg(path)
		r.b(')')
	})}
}

// To sets the column to v encoded as JSON.
func (c JSONBCol) To(v any) Assignment { return set(c.Col, v) }

// StringArrayCol is a text[] or varchar[] column.
type StringArrayCol struct{ Col }

// Has reports whether the array holds v.
func (c StringArrayCol) Has(v string) Pred {
	return predExpr{e: func(r *render) {
		r.arg(v)
		r.s(" = ANY(")
		r.col(c.Col)
		r.b(')')
	}}
}

func (c StringArrayCol) Overlaps(vs ...string) Pred    { return cmp(c.Col, " && ", vs) }
func (c StringArrayCol) ContainsAll(vs ...string) Pred { return cmp(c.Col, " @> ", vs) }
func (c StringArrayCol) ContainedBy(vs ...string) Pred { return cmp(c.Col, " <@ ", vs) }
func (c StringArrayCol) To(vs []string) Assignment     { return set(c.Col, vs) }

// Len is cardinality(col).
func (c StringArrayCol) Len() Agg { return Agg{fn: "cardinality", arg: c.Col, scalar: true} }

// Int64ArrayCol is a bigint[] column.
type Int64ArrayCol struct{ Col }

// Has reports whether the array holds v.
func (c Int64ArrayCol) Has(v int64) Pred {
	return predExpr{e: func(r *render) {
		r.arg(v)
		r.s(" = ANY(")
		r.col(c.Col)
		r.b(')')
	}}
}

func (c Int64ArrayCol) Overlaps(vs ...int64) Pred    { return cmp(c.Col, " && ", vs) }
func (c Int64ArrayCol) ContainsAll(vs ...int64) Pred { return cmp(c.Col, " @> ", vs) }
func (c Int64ArrayCol) ContainedBy(vs ...int64) Pred { return cmp(c.Col, " <@ ", vs) }
func (c Int64ArrayCol) To(vs []int64) Assignment     { return set(c.Col, vs) }

// TSVectorCol is a tsvector column.
type TSVectorCol struct{ Col }

// Matches is col @@ websearch_to_tsquery(q) with the database's default text search
// configuration.
func (c TSVectorCol) Matches(q string) Pred {
	return predExpr{e: func(r *render) {
		r.col(c.Col)
		r.s(" @@ websearch_to_tsquery(")
		r.arg(q)
		r.b(')')
	}}
}

// MatchesConfig is col @@ websearch_to_tsquery(config, q).
func (c TSVectorCol) MatchesConfig(config, q string) Pred {
	return predExpr{e: func(r *render) {
		r.col(c.Col)
		r.s(" @@ websearch_to_tsquery(")
		r.arg(config)
		r.s("::regconfig, ")
		r.arg(q)
		r.b(')')
	}}
}

// Rank is ts_rank(col, websearch_to_tsquery(q)).
func (c TSVectorCol) Rank(q string) Agg {
	return Agg{fn: "ts_rank", scalar: true, custom: func(r *render) {
		r.s("ts_rank(")
		r.col(c.Col)
		r.s(", websearch_to_tsquery(")
		r.arg(q)
		r.s("))")
	}}
}
