package keel

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/emoss08/keel/schema"
)

// Expr is a SQL expression. keel's column, aggregate and combinator values implement it; code
// outside keel builds expressions from those and from Raw.
type Expr interface {
	writeSQL(r *render)
}

// Pred is a boolean expression, accepted by Where, Having and the combinators.
type Pred interface {
	Expr
	pred()
}

type predExpr struct{ e func(r *render) }

func (p predExpr) writeSQL(r *render) { p.e(r) }
func (predExpr) pred()                {}

// And joins ps with AND. Nil predicates are skipped; And of nothing is TRUE.
func And(ps ...Pred) Pred { return boolList{ps: ps, op: " AND ", empty: "TRUE"} }

// Or joins ps with OR. Nil predicates are skipped; Or of nothing is FALSE.
func Or(ps ...Pred) Pred { return boolList{ps: ps, op: " OR ", empty: "FALSE"} }

type boolList struct {
	ps    []Pred
	op    string
	empty string
}

func (l boolList) writeSQL(r *render) {
	n := 0
	for _, p := range l.ps {
		if p != nil {
			n++
		}
	}
	if n == 0 {
		r.s(l.empty)
		return
	}
	r.b('(')
	first := true
	for _, p := range l.ps {
		if p == nil {
			continue
		}
		if !first {
			r.s(l.op)
		}
		first = false
		p.writeSQL(r)
	}
	r.b(')')
}

func (boolList) pred() {}

// Not negates p.
func Not(p Pred) Pred {
	return predExpr{e: func(r *render) {
		r.s("NOT (")
		p.writeSQL(r)
		r.b(')')
	}}
}

// When returns p when cond holds and nil otherwise. Where and the combinators skip nil, which makes
// optional filters one expression each.
func When(cond bool, p Pred) Pred {
	if !cond {
		return nil
	}
	return p
}

// True is the predicate TRUE.
func True() Pred { return predExpr{e: func(r *render) { r.s("TRUE") }} }

// Raw is SQL text that keel does not understand. Each ? in sql is replaced by the next argument:
// an Expr (a column, a subquery) is written in place with its alias resolved, any other value is
// bound as a parameter. Use ?? for a literal question mark.
func Raw(sql string, args ...any) RawExpr { return RawExpr{sql: sql, args: args} }

// RawExpr is the result of Raw. It is both an expression and a predicate.
type RawExpr struct {
	sql  string
	args []any
}

func (e RawExpr) writeSQL(r *render) {
	sql := e.sql
	n := 0
	for {
		i := strings.IndexByte(sql, '?')
		if i < 0 {
			r.s(sql)
			return
		}
		r.s(sql[:i])
		if i+1 < len(sql) && sql[i+1] == '?' {
			r.b('?')
			sql = sql[i+2:]
			continue
		}
		sql = sql[i+1:]
		if n >= len(e.args) {
			r.fail(errRawArgs)
			return
		}
		writeOperand(r, e.args[n], unsafe.Pointer(&e.args[n]), e.sql)
		n++
	}
}

func (RawExpr) pred() {}

// writeOperand writes v in place when it is an expression and binds it otherwise. slot is where
// the expression stores v and ctx the expression's text, so the same expression written twice
// binds v once.
func writeOperand(r *render, v any, slot unsafe.Pointer, ctx string) {
	if e, ok := v.(Expr); ok {
		e.writeSQL(r)
		return
	}
	r.argSlot(v, slot, ctx)
}

// Exists is EXISTS (q).
func Exists(q subquery) Pred {
	return predExpr{e: func(r *render) {
		r.s("EXISTS (")
		q.writeSubquery(r, true)
		r.b(')')
	}}
}

// NotExists is NOT EXISTS (q).
func NotExists(q subquery) Pred {
	return predExpr{e: func(r *render) {
		r.s("NOT EXISTS (")
		q.writeSubquery(r, true)
		r.b(')')
	}}
}

// subquery is a query used inside another statement. *Query[T] implements it.
type subquery interface {
	writeSubquery(r *render, existsOnly bool)
}

// Val binds v as a parameter where an expression is expected. The same Val written twice in a
// statement binds v once.
func Val(v any) Expr { return &valExpr{v: v} }

type valExpr struct{ v any }

func (e *valExpr) writeSQL(r *render) { r.argSlot(e.v, unsafe.Pointer(e), "") }

// Order is one ORDER BY term.
type Order struct {
	e     Expr
	col   *Col
	desc  bool
	nulls int8
}

// NullsFirst sorts NULL values first.
func (o Order) NullsFirst() Order { o.nulls = -1; return o }

// NullsLast sorts NULL values last.
func (o Order) NullsLast() Order { o.nulls = 1; return o }

func (o Order) writeSQL(r *render) {
	o.e.writeSQL(r)
	o.writeDirection(r)
}

// writeAt orders by the output at position pos of the select list.
func (o Order) writeAt(r *render, pos int) {
	r.s(strconv.Itoa(pos))
	o.writeDirection(r)
}

func (o Order) writeDirection(r *render) {
	if o.desc {
		r.s(" DESC")
	} else {
		r.s(" ASC")
	}
	switch o.nulls {
	case -1:
		r.s(" NULLS FIRST")
	case 1:
		r.s(" NULLS LAST")
	}
}

// Asc orders by e ascending.
func Asc(e Expr) Order { return Order{e: e} }

// Desc orders by e descending.
func Desc(e Expr) Order { return Order{e: e, desc: true} }

// Assignment is one SET term of an update, built by a column's To, ToNull or Add.
type Assignment struct {
	col Col
	e   func(r *render)
}

func escapeLike(s string) string {
	if !strings.ContainsAny(s, `%_\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Typed expressions. Each value kind keel types has an expression type (StringExpr, Int64Expr,
// Int32Expr, Float64Expr, NumericExpr, BoolExpr, TimeExpr, JSONBExpr) that compares to Go values of
// its kind, and a value interface (StringValue, Int64Value, …) that the kind's columns and
// expressions implement, so the argument of a typed function and the operand of a comparison
// between two expressions are checked by the compiler. Functions return the expression type of
// their result: s.ProNumber.Lower() is a StringExpr, keel.NowEpoch() an Int64Expr.

// StringValue is a text value: a string column (a generated enum or id column included) or a
// StringExpr.
type StringValue interface {
	Expr
	stringValue()
}

// Int64Value is a bigint value: an Int64Col or an Int64Expr.
type Int64Value interface {
	Expr
	int64Value()
}

// Int32Value is an integer value: an Int32Col or an Int32Expr.
type Int32Value interface {
	Expr
	int32Value()
}

// Float64Value is a double precision value: a Float64Col or a Float64Expr.
type Float64Value interface {
	Expr
	float64Value()
}

// NumericValue is a numeric value: a scanner column (AnyCol, how decimal columns are typed) or a
// NumericExpr.
type NumericValue interface {
	Expr
	numericValue()
}

// NumberValue is any number: a smallint, integer, bigint, double precision or scanner column, or an
// Int32Expr, Int64Expr, Float64Expr or NumericExpr.
type NumberValue interface {
	Expr
	numberValue()
}

// BoolValue is a boolean value: a BoolCol or a BoolExpr.
type BoolValue interface {
	Expr
	boolValue()
}

// TimeValue is a time: a TimeCol or a TimeExpr.
type TimeValue interface {
	Expr
	timeValue()
}

// JSONBValue is a jsonb value: a JSONBCol or a JSONBExpr.
type JSONBValue interface {
	Expr
	jsonbValue()
}

// kinded is an expression that knows the kind of value it yields.
type kinded interface {
	kind() (schema.Kind, bool)
}

// viewOf is the expression a typed view wraps: a column handle as its Col, so comparisons share
// placeholders and follow nullzero as the column's own methods do.
func viewOf(e Expr) Expr {
	switch x := e.(type) {
	case nil:
		return exprFunc(func(r *render) { r.fail(errf("typed view of a nil expression")) })
	case columnar:
		return x.column()
	}
	return e
}

// StringOf types e, an expression keel cannot type itself (a Raw, a Scalar, a DynExpr, a column of
// a Rows source), as text. It changes nothing in the SQL; CastText converts.
func StringOf(e Expr) StringExpr { return StringExpr{e: viewOf(e)} }

// Int64Of types e as bigint without changing the SQL; CastInt64 converts.
func Int64Of(e Expr) Int64Expr { return Int64Expr{e: viewOf(e)} }

// Int32Of types e as integer without changing the SQL.
func Int32Of(e Expr) Int32Expr { return Int32Expr{e: viewOf(e)} }

// Float64Of types e as double precision without changing the SQL; CastFloat64 converts.
func Float64Of(e Expr) Float64Expr { return Float64Expr{e: viewOf(e)} }

// NumericOf types e as numeric without changing the SQL; CastNumeric converts.
func NumericOf(e Expr) NumericExpr { return NumericExpr{e: viewOf(e)} }

// BoolOf types e as boolean without changing the SQL.
func BoolOf(e Expr) BoolExpr { return BoolExpr{e: viewOf(e)} }

// TimeOf types e as a time without changing the SQL.
func TimeOf(e Expr) TimeExpr { return TimeExpr{e: viewOf(e)} }

// JSONBOf types e as jsonb without changing the SQL.
func JSONBOf(e Expr) JSONBExpr { return JSONBExpr{e: viewOf(e)} }

// castLit is a value bound under an explicit cast, $n::typ, for an operand whose type nothing else
// in the statement fixes. Equal values under the same cast share a placeholder; a value that does
// not share by value binds once per literal.
type castLit struct {
	v   any
	typ string
}

func (l *castLit) writeSQL(r *render) {
	if shareable(l.v) {
		r.argTyped(l.v, l.typ)
	} else {
		r.argSlot(l.v, unsafe.Pointer(l), l.typ)
	}
	r.s("::")
	r.s(l.typ)
}

// Text binds s as a text value, $n::text, where a function or a comparison between expressions
// takes a StringValue.
func Text(s string) StringExpr { return StringExpr{e: &castLit{v: s, typ: "text"}} }

// Int64 binds n as a bigint value, $n::bigint.
func Int64(n int64) Int64Expr { return Int64Expr{e: &castLit{v: n, typ: "bigint"}} }

// Int32 binds n as an integer value, $n::integer.
func Int32(n int32) Int32Expr { return Int32Expr{e: &castLit{v: n, typ: "integer"}} }

// Float64 binds f as a double precision value, $n::double precision.
func Float64(f float64) Float64Expr {
	return Float64Expr{e: &castLit{v: f, typ: "double precision"}}
}

// Numeric binds v, a decimal or any value Postgres reads as numeric, as $n::numeric.
func Numeric(v any) NumericExpr { return NumericExpr{e: &castLit{v: v, typ: "numeric"}} }

// Bool binds b as a boolean value, $n::boolean.
func Bool(b bool) BoolExpr { return BoolExpr{e: &castLit{v: b, typ: "boolean"}} }

// Time binds t as a timestamptz value, $n::timestamptz.
func Time(t time.Time) TimeExpr { return TimeExpr{e: &castLit{v: t, typ: "timestamptz"}} }

// JSONB binds v encoded as JSON (with the DB's JSON codec) as $n::jsonb. A []byte or
// json.RawMessage is already JSON and binds as it is.
func JSONB(v any) JSONBExpr { return JSONBExpr{e: &jsonLit{v: v}} }

// jsonLit is a value bound as a jsonb document.
type jsonLit struct{ v any }

func (l *jsonLit) writeSQL(r *render) {
	var b []byte
	var err error
	switch x := l.v.(type) {
	case []byte:
		b = x
	case json.RawMessage:
		b = x
	default:
		b, err = r.db.marshalJSON(l.v)
	}
	if err != nil {
		r.fail(fmt.Errorf("keel: JSONB: %w", err))
		return
	}
	r.argSlot(string(b), unsafe.Pointer(l), "::jsonb")
	r.s("::jsonb")
}

// StringExpr is a computed text value: a string function, a jsonb path read as text, a typed view.
type StringExpr struct{ e Expr }

func (e StringExpr) writeSQL(r *render)         { e.e.writeSQL(r) }
func (StringExpr) stringValue()                 {}
func (StringExpr) kind() (schema.Kind, bool)    { return schema.KindString, true }
func (e StringExpr) Eq(v string) Pred           { return cmp(e.e, " = ", v) }
func (e StringExpr) Ne(v string) Pred           { return cmp(e.e, " <> ", v) }
func (e StringExpr) Gt(v string) Pred           { return cmp(e.e, " > ", v) }
func (e StringExpr) Gte(v string) Pred          { return cmp(e.e, " >= ", v) }
func (e StringExpr) Lt(v string) Pred           { return cmp(e.e, " < ", v) }
func (e StringExpr) Lte(v string) Pred          { return cmp(e.e, " <= ", v) }
func (e StringExpr) Between(lo, hi string) Pred { return between(e.e, lo, hi) }
func (e StringExpr) IsDistinctFrom(v string) Pred {
	return cmp(e.e, " IS DISTINCT FROM ", v)
}
func (e StringExpr) In(vs ...string) Pred         { return anyOf(e.e, vs, false) }
func (e StringExpr) NotIn(vs ...string) Pred      { return anyOf(e.e, vs, true) }
func (e StringExpr) EqExpr(o StringValue) Pred    { return binExpr(e.e, " = ", o) }
func (e StringExpr) NeExpr(o StringValue) Pred    { return binExpr(e.e, " <> ", o) }
func (e StringExpr) GtExpr(o StringValue) Pred    { return binExpr(e.e, " > ", o) }
func (e StringExpr) GteExpr(o StringValue) Pred   { return binExpr(e.e, " >= ", o) }
func (e StringExpr) LtExpr(o StringValue) Pred    { return binExpr(e.e, " < ", o) }
func (e StringExpr) LteExpr(o StringValue) Pred   { return binExpr(e.e, " <= ", o) }
func (e StringExpr) Like(p string) Pred           { return cmp(e.e, " LIKE ", p) }
func (e StringExpr) ILike(p string) Pred          { return cmp(e.e, " ILIKE ", p) }
func (e StringExpr) LikeExpr(p StringValue) Pred  { return binExpr(e.e, " LIKE ", p) }
func (e StringExpr) ILikeExpr(p StringValue) Pred { return binExpr(e.e, " ILIKE ", p) }
func (e StringExpr) Contains(s string) Pred       { return cmp(e.e, " LIKE ", "%"+escapeLike(s)+"%") }
func (e StringExpr) ContainsFold(s string) Pred {
	return cmp(e.e, " ILIKE ", "%"+escapeLike(s)+"%")
}
func (e StringExpr) HasPrefix(s string) Pred     { return cmp(e.e, " LIKE ", escapeLike(s)+"%") }
func (e StringExpr) HasPrefixFold(s string) Pred { return cmp(e.e, " ILIKE ", escapeLike(s)+"%") }
func (e StringExpr) HasSuffix(s string) Pred     { return cmp(e.e, " LIKE ", "%"+escapeLike(s)) }
func (e StringExpr) Match(re string) Pred        { return cmp(e.e, " ~ ", re) }
func (e StringExpr) MatchFold(re string) Pred    { return cmp(e.e, " ~* ", re) }
func (e StringExpr) IsNull() Pred                { return suffix(e.e, " IS NULL") }
func (e StringExpr) IsNotNull() Pred             { return suffix(e.e, " IS NOT NULL") }
func (e StringExpr) Asc() Order                  { return Order{e: e.e} }
func (e StringExpr) Desc() Order                 { return Order{e: e.e, desc: true} }
func (e StringExpr) Min() Agg                    { return Agg{fn: "min", arg: e} }
func (e StringExpr) Max() Agg                    { return Agg{fn: "max", arg: e} }

// Int64Expr is a computed bigint value.
type Int64Expr struct{ e Expr }

func (e Int64Expr) writeSQL(r *render)          { e.e.writeSQL(r) }
func (Int64Expr) int64Value()                   {}
func (Int64Expr) numberValue()                  {}
func (Int64Expr) kind() (schema.Kind, bool)     { return schema.KindInt64, true }
func (e Int64Expr) Eq(v int64) Pred             { return cmp(e.e, " = ", v) }
func (e Int64Expr) Ne(v int64) Pred             { return cmp(e.e, " <> ", v) }
func (e Int64Expr) Gt(v int64) Pred             { return cmp(e.e, " > ", v) }
func (e Int64Expr) Gte(v int64) Pred            { return cmp(e.e, " >= ", v) }
func (e Int64Expr) Lt(v int64) Pred             { return cmp(e.e, " < ", v) }
func (e Int64Expr) Lte(v int64) Pred            { return cmp(e.e, " <= ", v) }
func (e Int64Expr) Between(lo, hi int64) Pred   { return between(e.e, lo, hi) }
func (e Int64Expr) IsDistinctFrom(v int64) Pred { return cmp(e.e, " IS DISTINCT FROM ", v) }
func (e Int64Expr) In(vs ...int64) Pred         { return anyOf(e.e, vs, false) }
func (e Int64Expr) NotIn(vs ...int64) Pred      { return anyOf(e.e, vs, true) }
func (e Int64Expr) EqExpr(o Int64Value) Pred    { return binExpr(e.e, " = ", o) }
func (e Int64Expr) NeExpr(o Int64Value) Pred    { return binExpr(e.e, " <> ", o) }
func (e Int64Expr) GtExpr(o Int64Value) Pred    { return binExpr(e.e, " > ", o) }
func (e Int64Expr) GteExpr(o Int64Value) Pred   { return binExpr(e.e, " >= ", o) }
func (e Int64Expr) LtExpr(o Int64Value) Pred    { return binExpr(e.e, " < ", o) }
func (e Int64Expr) LteExpr(o Int64Value) Pred   { return binExpr(e.e, " <= ", o) }
func (e Int64Expr) IsNull() Pred                { return suffix(e.e, " IS NULL") }
func (e Int64Expr) IsNotNull() Pred             { return suffix(e.e, " IS NOT NULL") }
func (e Int64Expr) Asc() Order                  { return Order{e: e.e} }
func (e Int64Expr) Desc() Order                 { return Order{e: e.e, desc: true} }
func (e Int64Expr) Sum() Agg                    { return Agg{fn: "sum", arg: e} }
func (e Int64Expr) Avg() Agg                    { return Agg{fn: "avg", arg: e} }
func (e Int64Expr) Min() Agg                    { return Agg{fn: "min", arg: e} }
func (e Int64Expr) Max() Agg                    { return Agg{fn: "max", arg: e} }

// Int32Expr is a computed integer value: a length, a position, a bucket number.
type Int32Expr struct{ e Expr }

func (e Int32Expr) writeSQL(r *render)          { e.e.writeSQL(r) }
func (Int32Expr) int32Value()                   {}
func (Int32Expr) numberValue()                  {}
func (Int32Expr) kind() (schema.Kind, bool)     { return schema.KindInt32, true }
func (e Int32Expr) Eq(v int32) Pred             { return cmp(e.e, " = ", v) }
func (e Int32Expr) Ne(v int32) Pred             { return cmp(e.e, " <> ", v) }
func (e Int32Expr) Gt(v int32) Pred             { return cmp(e.e, " > ", v) }
func (e Int32Expr) Gte(v int32) Pred            { return cmp(e.e, " >= ", v) }
func (e Int32Expr) Lt(v int32) Pred             { return cmp(e.e, " < ", v) }
func (e Int32Expr) Lte(v int32) Pred            { return cmp(e.e, " <= ", v) }
func (e Int32Expr) Between(lo, hi int32) Pred   { return between(e.e, lo, hi) }
func (e Int32Expr) IsDistinctFrom(v int32) Pred { return cmp(e.e, " IS DISTINCT FROM ", v) }
func (e Int32Expr) In(vs ...int32) Pred         { return anyOf(e.e, vs, false) }
func (e Int32Expr) NotIn(vs ...int32) Pred      { return anyOf(e.e, vs, true) }
func (e Int32Expr) EqExpr(o Int32Value) Pred    { return binExpr(e.e, " = ", o) }
func (e Int32Expr) NeExpr(o Int32Value) Pred    { return binExpr(e.e, " <> ", o) }
func (e Int32Expr) GtExpr(o Int32Value) Pred    { return binExpr(e.e, " > ", o) }
func (e Int32Expr) GteExpr(o Int32Value) Pred   { return binExpr(e.e, " >= ", o) }
func (e Int32Expr) LtExpr(o Int32Value) Pred    { return binExpr(e.e, " < ", o) }
func (e Int32Expr) LteExpr(o Int32Value) Pred   { return binExpr(e.e, " <= ", o) }
func (e Int32Expr) IsNull() Pred                { return suffix(e.e, " IS NULL") }
func (e Int32Expr) IsNotNull() Pred             { return suffix(e.e, " IS NOT NULL") }
func (e Int32Expr) Asc() Order                  { return Order{e: e.e} }
func (e Int32Expr) Desc() Order                 { return Order{e: e.e, desc: true} }
func (e Int32Expr) Sum() Agg                    { return Agg{fn: "sum", arg: e} }
func (e Int32Expr) Avg() Agg                    { return Agg{fn: "avg", arg: e} }
func (e Int32Expr) Min() Agg                    { return Agg{fn: "min", arg: e} }
func (e Int32Expr) Max() Agg                    { return Agg{fn: "max", arg: e} }

// Float64Expr is a computed double precision value: a date part, a logarithm, a rounded float.
type Float64Expr struct{ e Expr }

func (e Float64Expr) writeSQL(r *render)            { e.e.writeSQL(r) }
func (Float64Expr) float64Value()                   {}
func (Float64Expr) numberValue()                    {}
func (Float64Expr) kind() (schema.Kind, bool)       { return schema.KindFloat64, true }
func (e Float64Expr) Eq(v float64) Pred             { return cmp(e.e, " = ", v) }
func (e Float64Expr) Ne(v float64) Pred             { return cmp(e.e, " <> ", v) }
func (e Float64Expr) Gt(v float64) Pred             { return cmp(e.e, " > ", v) }
func (e Float64Expr) Gte(v float64) Pred            { return cmp(e.e, " >= ", v) }
func (e Float64Expr) Lt(v float64) Pred             { return cmp(e.e, " < ", v) }
func (e Float64Expr) Lte(v float64) Pred            { return cmp(e.e, " <= ", v) }
func (e Float64Expr) Between(lo, hi float64) Pred   { return between(e.e, lo, hi) }
func (e Float64Expr) IsDistinctFrom(v float64) Pred { return cmp(e.e, " IS DISTINCT FROM ", v) }
func (e Float64Expr) EqExpr(o Float64Value) Pred    { return binExpr(e.e, " = ", o) }
func (e Float64Expr) NeExpr(o Float64Value) Pred    { return binExpr(e.e, " <> ", o) }
func (e Float64Expr) GtExpr(o Float64Value) Pred    { return binExpr(e.e, " > ", o) }
func (e Float64Expr) GteExpr(o Float64Value) Pred   { return binExpr(e.e, " >= ", o) }
func (e Float64Expr) LtExpr(o Float64Value) Pred    { return binExpr(e.e, " < ", o) }
func (e Float64Expr) LteExpr(o Float64Value) Pred   { return binExpr(e.e, " <= ", o) }
func (e Float64Expr) IsNull() Pred                  { return suffix(e.e, " IS NULL") }
func (e Float64Expr) IsNotNull() Pred               { return suffix(e.e, " IS NOT NULL") }
func (e Float64Expr) Asc() Order                    { return Order{e: e.e} }
func (e Float64Expr) Desc() Order                   { return Order{e: e.e, desc: true} }
func (e Float64Expr) Sum() Agg                      { return Agg{fn: "sum", arg: e} }
func (e Float64Expr) Avg() Agg                      { return Agg{fn: "avg", arg: e} }
func (e Float64Expr) Min() Agg                      { return Agg{fn: "min", arg: e} }
func (e Float64Expr) Max() Agg                      { return Agg{fn: "max", arg: e} }

// NumericExpr is a computed numeric value. Its comparisons take any value Postgres reads as numeric
// (a decimal type, an integer, a float), as an AnyCol's do.
type NumericExpr struct{ e Expr }

func (e NumericExpr) writeSQL(r *render)      { e.e.writeSQL(r) }
func (NumericExpr) numericValue()             {}
func (NumericExpr) numberValue()              {}
func (NumericExpr) kind() (schema.Kind, bool) { return schema.KindOther, true }
func (e NumericExpr) Eq(v any) Pred           { return cmp(e.e, " = ", v) }
func (e NumericExpr) Ne(v any) Pred           { return cmp(e.e, " <> ", v) }
func (e NumericExpr) Gt(v any) Pred           { return cmp(e.e, " > ", v) }
func (e NumericExpr) Gte(v any) Pred          { return cmp(e.e, " >= ", v) }
func (e NumericExpr) Lt(v any) Pred           { return cmp(e.e, " < ", v) }
func (e NumericExpr) Lte(v any) Pred          { return cmp(e.e, " <= ", v) }
func (e NumericExpr) Between(lo, hi any) Pred { return between(e.e, lo, hi) }
func (e NumericExpr) IsDistinctFrom(v any) Pred {
	return cmp(e.e, " IS DISTINCT FROM ", v)
}
func (e NumericExpr) EqExpr(o NumericValue) Pred  { return binExpr(e.e, " = ", o) }
func (e NumericExpr) NeExpr(o NumericValue) Pred  { return binExpr(e.e, " <> ", o) }
func (e NumericExpr) GtExpr(o NumericValue) Pred  { return binExpr(e.e, " > ", o) }
func (e NumericExpr) GteExpr(o NumericValue) Pred { return binExpr(e.e, " >= ", o) }
func (e NumericExpr) LtExpr(o NumericValue) Pred  { return binExpr(e.e, " < ", o) }
func (e NumericExpr) LteExpr(o NumericValue) Pred { return binExpr(e.e, " <= ", o) }
func (e NumericExpr) IsNull() Pred                { return suffix(e.e, " IS NULL") }
func (e NumericExpr) IsNotNull() Pred             { return suffix(e.e, " IS NOT NULL") }
func (e NumericExpr) Asc() Order                  { return Order{e: e.e} }
func (e NumericExpr) Desc() Order                 { return Order{e: e.e, desc: true} }
func (e NumericExpr) Sum() Agg                    { return Agg{fn: "sum", arg: e} }
func (e NumericExpr) Avg() Agg                    { return Agg{fn: "avg", arg: e} }
func (e NumericExpr) Min() Agg                    { return Agg{fn: "min", arg: e} }
func (e NumericExpr) Max() Agg                    { return Agg{fn: "max", arg: e} }

// BoolExpr is a computed boolean value. It is a predicate, so Where and the combinators take it.
type BoolExpr struct{ e Expr }

func (e BoolExpr) writeSQL(r *render)      { e.e.writeSQL(r) }
func (BoolExpr) pred()                     {}
func (BoolExpr) boolValue()                {}
func (BoolExpr) kind() (schema.Kind, bool) { return schema.KindBool, true }
func (e BoolExpr) Eq(v bool) Pred          { return cmp(e.e, " = ", v) }
func (e BoolExpr) EqExpr(o BoolValue) Pred { return binExpr(e.e, " = ", o) }
func (e BoolExpr) IsTrue() Pred            { return suffix(e.e, " IS TRUE") }
func (e BoolExpr) IsFalse() Pred           { return suffix(e.e, " IS FALSE") }
func (e BoolExpr) IsNull() Pred            { return suffix(e.e, " IS NULL") }
func (e BoolExpr) IsNotNull() Pred         { return suffix(e.e, " IS NOT NULL") }
func (e BoolExpr) Asc() Order              { return Order{e: e.e} }
func (e BoolExpr) Desc() Order             { return Order{e: e.e, desc: true} }

// TimeExpr is a computed time: now(), to_timestamp of epoch seconds, a time in a zone, a truncated
// time.
type TimeExpr struct{ e Expr }

func (e TimeExpr) writeSQL(r *render)            { e.e.writeSQL(r) }
func (TimeExpr) timeValue()                      {}
func (TimeExpr) kind() (schema.Kind, bool)       { return schema.KindTime, true }
func (e TimeExpr) Eq(v time.Time) Pred           { return cmp(e.e, " = ", v) }
func (e TimeExpr) Ne(v time.Time) Pred           { return cmp(e.e, " <> ", v) }
func (e TimeExpr) Gt(v time.Time) Pred           { return cmp(e.e, " > ", v) }
func (e TimeExpr) Gte(v time.Time) Pred          { return cmp(e.e, " >= ", v) }
func (e TimeExpr) Lt(v time.Time) Pred           { return cmp(e.e, " < ", v) }
func (e TimeExpr) Lte(v time.Time) Pred          { return cmp(e.e, " <= ", v) }
func (e TimeExpr) Between(lo, hi time.Time) Pred { return between(e.e, lo, hi) }
func (e TimeExpr) EqExpr(o TimeValue) Pred       { return binExpr(e.e, " = ", o) }
func (e TimeExpr) NeExpr(o TimeValue) Pred       { return binExpr(e.e, " <> ", o) }
func (e TimeExpr) GtExpr(o TimeValue) Pred       { return binExpr(e.e, " > ", o) }
func (e TimeExpr) GteExpr(o TimeValue) Pred      { return binExpr(e.e, " >= ", o) }
func (e TimeExpr) LtExpr(o TimeValue) Pred       { return binExpr(e.e, " < ", o) }
func (e TimeExpr) LteExpr(o TimeValue) Pred      { return binExpr(e.e, " <= ", o) }
func (e TimeExpr) IsNull() Pred                  { return suffix(e.e, " IS NULL") }
func (e TimeExpr) IsNotNull() Pred               { return suffix(e.e, " IS NOT NULL") }
func (e TimeExpr) Asc() Order                    { return Order{e: e.e} }
func (e TimeExpr) Desc() Order                   { return Order{e: e.e, desc: true} }
func (e TimeExpr) Min() Agg                      { return Agg{fn: "min", arg: e} }
func (e TimeExpr) Max() Agg                      { return Agg{fn: "max", arg: e} }

// JSONBExpr is a computed jsonb value: a built object or array, a document with a value set.
type JSONBExpr struct{ e Expr }

func (e JSONBExpr) writeSQL(r *render)      { e.e.writeSQL(r) }
func (JSONBExpr) jsonbValue()               {}
func (JSONBExpr) kind() (schema.Kind, bool) { return schema.KindJSON, true }

// Contains is e @> v, with v encoded as JSON.
func (e JSONBExpr) Contains(v any) Pred { return cmp(e.e, " @> ", v) }

// ContainsExpr is e @> o.
func (e JSONBExpr) ContainsExpr(o JSONBValue) Pred { return binExpr(e.e, " @> ", o) }

// HasKey is e ? key.
func (e JSONBExpr) HasKey(key string) Pred { return cmp(e.e, " ? ", key) }

// Text is e #>> path: the value at path as text.
func (e JSONBExpr) Text(path ...string) StringExpr {
	if path == nil {
		path = []string{}
	}
	return StringExpr{e: exprFunc(func(r *render) {
		r.b('(')
		e.e.writeSQL(r)
		r.s(" #>> ")
		r.argSlot(path, unsafe.Pointer(unsafe.SliceData(path)), " #>> ")
		r.b(')')
	})}
}

func (e JSONBExpr) IsNull() Pred    { return suffix(e.e, " IS NULL") }
func (e JSONBExpr) IsNotNull() Pred { return suffix(e.e, " IS NOT NULL") }
